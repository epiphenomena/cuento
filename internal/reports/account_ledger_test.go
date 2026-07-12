package reports_test

// p15.6 account-ledger report tests. Every number below is HAND-DERIVED from the
// synthetic fixture (PLAN Appendix D, internal/testutil/fixture) — the fixture's
// Expected.AccountBalances is the oracle for the closing figures, and
// transactions.go is the oracle for the in-range lines. The ledger's defining
// identity is opening + Σ(range lines) == closing, cross-checked here against the
// as-of balance the fixture independently pins (BalancesAsOf vs DrillSplits).
//
// GOLDEN account: Checking MX (single currency MXN, MULTI-FUND) over a range that
// STARTS MID-FIXTURE, so the opening balance is nonzero (prior activity is folded in,
// only in-range lines listed) and the fund column shows both a restricted fund (Beca
// Agua) and the Unrestricted group:
//
//	from = 2025-05-01, to = 2026-06-30
//	opening (as of 2025-04-30) = 30,000,000 (2025-01-01 open) + 10,000,000
//	                             (2025-04-01 Beca Agua grant receipt) = 40,000,000 MXN
//	line 2025-05-10 Program supplies (mixed funding):
//	   -300,000 (Beca Agua)      running 39,700,000
//	   -200,000 (Unrestricted)   running 39,500,000
//	closing (as of 2026-06-30) = 39,500,000 MXN  == Expected.AccountBalances{CheckingMX,MXN}
//	opening + Σlines = 40,000,000 + (-300,000 - 200,000) = 39,500,000 == closing ✓

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// accountLedgerReport fetches the registered account-ledger report from the default
// registry (proving it IS registered under its id).
func accountLedgerReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.AccountLedgerReportID)
	if !ok {
		t.Fatalf("account-ledger report %q not registered in Default()", reports.AccountLedgerReportID)
	}
	return rep
}

// ledgerGoldenParams are the FIXED params the golden runs at: root scope (full
// consolidation), Checking MX, the mid-fixture range above, lang en. No target
// currency — the ledger prints native amounts.
func ledgerGoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:   f.IDs.Root,
		Account: f.IDs.CheckingMX,
		From:    "2025-05-01",
		To:      f.Expected.AsOf, // 2026-06-30
		Lang:    "en",
	}
}

// TestAccountLedgerGolden runs the account ledger over Checking MX at the pinned
// mid-fixture range, asserts the ledger identity (opening + Σlines == closing == the
// fixture oracle) per currency, checks the fund column and the running balance
// line-by-line, and compares the rendered text + CSV to committed goldens.
func TestAccountLedgerGolden(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()

	rep := accountLedgerReport(t)
	p := ledgerGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run account ledger: %v", err)
	}

	// --- The ledger identity, re-derived from the report's OWN emitted rows: the
	// opening row's balance + the sum of the in-range line amounts == the closing row's
	// balance, per currency. Re-summing the emitted cells (not trusting a single figure)
	// catches a dropped/duplicated line.
	openBal := ledgerOpening(t, table, "MXN")
	closeBal := ledgerClosing(t, table, "MXN")
	lineSum := ledgerLineAmountSum(table, "MXN")
	if openBal+lineSum != closeBal {
		t.Errorf("ledger identity broken: opening %d + Σlines %d = %d, want closing %d",
			openBal, lineSum, openBal+lineSum, closeBal)
	}

	// --- Hand-derived figures (NOT read from report output).
	if openBal != 40_000_000 {
		t.Errorf("opening balance = %d, want 40,000,000 (30,000,000 open + 10,000,000 Beca Agua grant)", openBal)
	}
	if lineSum != -500_000 {
		t.Errorf("in-range Σlines = %d, want -500,000 (-300,000 + -200,000)", lineSum)
	}
	if closeBal != 39_500_000 {
		t.Errorf("closing balance = %d, want 39,500,000", closeBal)
	}
	// --- Closing equals the fixture oracle (an INDEPENDENT balance query): the report's
	// closing figure must match Expected.AccountBalances{CheckingMX,MXN}.
	if want := expectedBalance(t, f, f.IDs.CheckingMX, "MXN"); closeBal != want {
		t.Errorf("closing %d != fixture oracle balance %d (Checking MX / MXN)", closeBal, want)
	}

	// --- Exactly two in-range DATA lines, in date order, with the RIGHT funds and a
	// correct running balance line-by-line.
	lines := ledgerDataRows(table)
	if len(lines) != 2 {
		t.Fatalf("in-range lines = %d, want 2", len(lines))
	}
	// Line 1: Beca Agua -300,000, running 39,700,000.
	assertLine(t, lines[0], "Beca Agua 2025", -300_000, 39_700_000)
	// Line 2: Unrestricted -200,000, running 39,500,000.
	assertLine(t, lines[1], "Unrestricted", -200_000, 39_500_000)

	// --- Golden artifacts: aligned text dump + machine CSV.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)

	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	checkGolden(t, "account_ledger.txt", []byte(textDump))
	checkGolden(t, "account_ledger.csv", csvBuf.Bytes())
}

// TestAccountLedgerRangeBoundary: a range starting at the fixture's beginning INCLUDES
// the prior transactions the mid-fixture golden excludes — the opening balance is then
// zero (no activity before) and the earlier lines appear. This proves the range bound
// is honored: opening reflects only pre-range activity, only in-range lines are listed.
func TestAccountLedgerRangeBoundary(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)

	full := reports.Params{Scope: f.IDs.Root, Account: f.IDs.CheckingMX, From: "2025-01-01", To: f.Expected.AsOf, Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, full), full)
	if err != nil {
		t.Fatalf("run full range: %v", err)
	}

	// Opening at 2024-12-31 = 0 (no Checking MX activity before 2025-01-01).
	if open := ledgerOpening(t, table, "MXN"); open != 0 {
		t.Errorf("full-range opening = %d, want 0", open)
	}
	// The full range lists FOUR MXN lines (open, grant, and the two mixed-supply legs) —
	// more than the mid-fixture range's two, proving the earlier txns are now included.
	lines := ledgerDataRows(table)
	if len(lines) != 4 {
		t.Errorf("full-range lines = %d, want 4 (mid-range excludes the first two)", len(lines))
	}
	// Identity still holds and closing is unchanged (39,500,000).
	if open, close, sum := ledgerOpening(t, table, "MXN"), ledgerClosing(t, table, "MXN"), ledgerLineAmountSum(table, "MXN"); open+sum != close || close != 39_500_000 {
		t.Errorf("full-range identity/closing wrong: open %d + Σ %d = %d, close %d (want close 39,500,000)", open, sum, open+sum, close)
	}
}

// TestAccountLedgerMultiCurrency: FX Clearing holds USD and MXN; the ledger renders one
// SECTION per currency, each with its own opening/lines/closing and an INDEPENDENT
// running balance — currencies are never mixed in one running total. Each section's
// closing equals the fixture oracle for that currency.
func TestAccountLedgerMultiCurrency(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)

	p := reports.Params{Scope: f.IDs.Root, Account: f.IDs.FXClearing, From: "2025-01-01", To: f.Expected.AsOf, Lang: "en"}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run FX Clearing ledger: %v", err)
	}

	for _, tc := range []struct {
		ccy  string
		want int64
	}{
		{"USD", 974_000}, // +1,000,000 intercompany - 26,000 FX transfer
		{"MXN", 500_000}, // +500,000 FX transfer in
	} {
		open := ledgerOpening(t, table, tc.ccy)
		close := ledgerClosing(t, table, tc.ccy)
		sum := ledgerLineAmountSum(table, tc.ccy)
		if open+sum != close {
			t.Errorf("%s identity broken: open %d + Σ %d = %d, close %d", tc.ccy, open, sum, open+sum, close)
		}
		if close != tc.want {
			t.Errorf("%s closing = %d, want %d", tc.ccy, close, tc.want)
		}
		if want := expectedBalance(t, f, f.IDs.FXClearing, tc.ccy); close != want {
			t.Errorf("%s closing %d != fixture oracle %d", tc.ccy, close, want)
		}
	}

	// The two currencies' running balances never bleed together: every line's running
	// cell is in the SAME currency as its amount cell (per-section running total).
	for _, row := range table.Rows {
		if row.Kind != reports.RowData {
			continue
		}
		amt := row.Cells[3]
		bal := row.Cells[4]
		if amt.Currency != bal.Currency {
			t.Errorf("line mixes currencies: amount %s, running %s", amt.Currency, bal.Currency)
		}
	}
}

// TestAccountLedgerNoAccount: with no account chosen (Account == 0), the report returns
// an empty Table (the framework's nothing-to-show rule), so a bare hit renders 200 with
// just the params form (the permission-matrix / scope-selector path).
func TestAccountLedgerNoAccount(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)

	p := reports.Params{Scope: f.IDs.Root, From: "2025-01-01", To: f.Expected.AsOf, Lang: "en"} // Account left 0
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run with no account: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("no-account ledger has %d rows, want 0 (empty table)", len(table.Rows))
	}
	// The columns are still declared (the empty-table render shows the header).
	if len(table.Columns) != 5 {
		t.Errorf("no-account ledger columns = %d, want 5", len(table.Columns))
	}
}

// TestAccountLedgerLineLinksAndDrill: each in-range LINE cell carries a txn link
// (Cell.TxnID, the p12.4 editor), and the opening/closing balance cells carry an as-of
// Drill (p15.3d) whose filter reconstructs that cumulative balance.
func TestAccountLedgerLineLinksAndDrill(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)
	p := ledgerGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Every DATA line's date cell links to its transaction (nonzero TxnID).
	for i, row := range ledgerDataRows(table) {
		if row.Cells[0].TxnID == 0 {
			t.Errorf("line %d date cell has no txn link (TxnID == 0)", i)
		}
		// A line does not also carry a Drill (TxnID and Drill are distinct mechanisms).
		if row.Cells[0].Drill != nil {
			t.Errorf("line %d date cell unexpectedly drillable", i)
		}
	}

	// Opening + closing balance cells (last cell of the subtotal/total rows) carry an
	// as-of Drill for this account+currency.
	var sawOpen, sawClose bool
	for _, row := range table.Rows {
		last := row.Cells[len(row.Cells)-1]
		switch row.Kind {
		case reports.RowSubtotal: // opening
			sawOpen = true
			if last.Drill == nil || last.Drill.Mode != reports.DrillAsOf || last.Drill.AsOf != "2025-04-30" {
				t.Errorf("opening cell drill = %+v, want as-of 2025-04-30", last.Drill)
			}
			if len(last.Drill.AccountIDs) != 1 || last.Drill.AccountIDs[0] != f.IDs.CheckingMX {
				t.Errorf("opening drill accounts = %v, want [CheckingMX]", last.Drill.AccountIDs)
			}
		case reports.RowTotal: // closing
			sawClose = true
			if last.Drill == nil || last.Drill.Mode != reports.DrillAsOf || last.Drill.AsOf != f.Expected.AsOf {
				t.Errorf("closing cell drill = %+v, want as-of %s", last.Drill, f.Expected.AsOf)
			}
		}
	}
	if !sawOpen || !sawClose {
		t.Errorf("missing opening (%v) or closing (%v) balance row", sawOpen, sawClose)
	}
}

// TestAccountLedgerDrillReconciles: the RECONCILIATION invariant (p15.3d) on the
// opening/closing balance cells — the signed sum of the splits the cell's Drill
// selects (store.DrillSplits, the SAME query the drill route runs) EQUALS the cell's
// figure. This exercises the drill QUERY (not just the descriptor shape), the way
// TestBalanceSheetDrillReconciles / TestIncomeStatementDrillReconciles do: the store
// is the oracle, the report cell must match it. HAND-VERIFIED anchors: opening (as of
// 2025-04-30) = 40,000,000; closing (as of 2026-06-30) = 39,500,000.
func TestAccountLedgerDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)
	p := ledgerGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	sawOpen, sawClose := false, false
	for _, row := range table.Rows {
		if row.Kind != reports.RowSubtotal && row.Kind != reports.RowTotal {
			continue
		}
		cell := row.Cells[len(row.Cells)-1] // the balance cell
		d := cell.Drill
		if d == nil {
			t.Fatalf("framing cell (kind %v) is not drillable", row.Kind)
		}
		// Build the store filter FROM the cell's Drill (mirrors the web drill handler),
		// fetch the contributing splits, and sum them — must equal the cell figure.
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
			splits, err := f.Store.DrillSplits(ctx, filter)
			if err != nil {
				t.Fatalf("drill splits: %v", err)
			}
			for _, s := range splits {
				sum += s.Amount
			}
		}
		if sum != cell.Minor {
			t.Errorf("drill (kind %v, as-of %s) sum = %d, want cell figure %d", row.Kind, d.AsOf, sum, cell.Minor)
		}
		switch row.Kind {
		case reports.RowSubtotal:
			sawOpen = true
			if cell.Minor != 40_000_000 {
				t.Errorf("opening cell = %d, want 40,000,000", cell.Minor)
			}
		case reports.RowTotal:
			sawClose = true
			if cell.Minor != 39_500_000 {
				t.Errorf("closing cell = %d, want 39,500,000", cell.Minor)
			}
		}
	}
	if !sawOpen || !sawClose {
		t.Errorf("did not reconcile both opening (%v) and closing (%v)", sawOpen, sawClose)
	}
}

// TestAccountLedgerCSVParses: the ledger's CSV output parses back to well-formed
// records; re-summing the amount column over the DATA rows reproduces Σlines, and the
// header is the localized column set.
func TestAccountLedgerCSVParses(t *testing.T) {
	f := fixture.New(t)
	ctx := context.Background()
	rep := accountLedgerReport(t)
	p := ledgerGoldenParams(f)
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
	wantHeader := []string{"Date", "Description", "Fund", "Amount", "Balance"}
	for i, h := range wantHeader {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
	// Re-sum the Amount column (index 3) over the two data lines (skip the opening/closing
	// rows, whose Description is the localized label and whose Amount cell is blank).
	var sum int64
	for _, rec := range recs[1:] {
		if rec[0] == "" || rec[3] == "" {
			continue
		}
		if rec[1] == "Opening balance" || rec[1] == "Closing balance" {
			continue
		}
		sum += parseMinor(t, rec[3])
	}
	if sum != -500_000 {
		t.Errorf("csv amount re-sum = %d, want -500,000", sum)
	}
}

// --- ledger-specific inspection helpers -------------------------------------

// ledgerDataRows returns the DATA (in-range line) rows of a ledger table.
func ledgerDataRows(t reports.Table) []reports.Row {
	var out []reports.Row
	for _, row := range t.Rows {
		if row.Kind == reports.RowData {
			out = append(out, row)
		}
	}
	return out
}

// ledgerOpening returns the opening-balance figure (the RowSubtotal row's balance cell)
// for currency ccy.
func ledgerOpening(t *testing.T, tbl reports.Table, ccy string) int64 {
	t.Helper()
	return ledgerFramingBalance(t, tbl, reports.RowSubtotal, ccy, "opening")
}

// ledgerClosing returns the closing-balance figure (the RowTotal row's balance cell) for
// currency ccy.
func ledgerClosing(t *testing.T, tbl reports.Table, ccy string) int64 {
	t.Helper()
	return ledgerFramingBalance(t, tbl, reports.RowTotal, ccy, "closing")
}

// ledgerFramingBalance finds the opening/closing framing row of kind k whose balance
// cell is in ccy and returns its minor amount (the last cell holds the balance).
func ledgerFramingBalance(t *testing.T, tbl reports.Table, k reports.RowKind, ccy, which string) int64 {
	t.Helper()
	for _, row := range tbl.Rows {
		if row.Kind != k {
			continue
		}
		bal := row.Cells[len(row.Cells)-1]
		if bal.Kind == reports.CellMoney && !bal.Blank && bal.Currency == ccy {
			return bal.Minor
		}
	}
	t.Fatalf("no %s balance row for %s", which, ccy)
	return 0
}

// ledgerLineAmountSum sums the amount cells (col 3) of the DATA lines in currency ccy.
func ledgerLineAmountSum(t reports.Table, ccy string) int64 {
	var sum int64
	for _, row := range t.Rows {
		if row.Kind != reports.RowData {
			continue
		}
		amt := row.Cells[3]
		if amt.Kind == reports.CellMoney && !amt.Blank && amt.Currency == ccy {
			sum += amt.Minor
		}
	}
	return sum
}

// assertLine checks one ledger DATA row's fund label/name, amount, and running balance.
// wantFund is the localized/verbatim fund text ("Unrestricted" or the fund name).
func assertLine(t *testing.T, row reports.Row, wantFund string, wantAmount, wantRunning int64) {
	t.Helper()
	fund := row.Cells[2]
	fundText := fund.Text
	if fund.Kind == reports.CellLabel {
		fundText = goldenLocalize(fund.Text)
	}
	if fundText != wantFund {
		t.Errorf("line fund = %q, want %q", fundText, wantFund)
	}
	if amt := row.Cells[3]; amt.Minor != wantAmount {
		t.Errorf("line amount = %d, want %d", amt.Minor, wantAmount)
	}
	if bal := row.Cells[4]; bal.Minor != wantRunning {
		t.Errorf("line running balance = %d, want %d", bal.Minor, wantRunning)
	}
}

// expectedBalance returns the fixture's hand-computed as-of balance for (account,
// currency) from Expected.AccountBalances (the ROOT-scope oracle).
func expectedBalance(t *testing.T, f *fixture.Fixture, account int64, ccy string) int64 {
	t.Helper()
	for _, b := range f.Expected.AccountBalances {
		if b.Account == account && b.Currency == ccy {
			return b.Amount
		}
	}
	t.Fatalf("no expected balance for account %d / %s", account, ccy)
	return 0
}
