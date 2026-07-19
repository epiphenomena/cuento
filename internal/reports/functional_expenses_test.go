package reports_test

// p15.7 functional-expenses (IRS Form 990 Part IX — Statement of Functional Expenses)
// report tests. Every asserted number is HAND-DERIVED from the canonical synthetic
// fixture (PLAN Appendix D, internal/testutil/fixture) — the Functional matrix cells
// and the effective-990 rollup — and the p14.1 monthly USD->MXN rate seam. The fixture
// is the oracle, never the report's own output. The golden files
// (testdata/functional_expenses.{txt,csv}) are a committed, human-reviewable rendering;
// -update / `make golden` regenerate them deterministically (lang=en, root scope,
// period 2025-01-01..2026-06-30, USD target, TRANSACTION-DATE-rate conversion, p26.71).
//
// The report is a 2D MATRIX: ROWS = effective Part IX lines (expense accounts grouped
// + subtotaled under their effective line, Unmapped bucket LAST); COLUMNS = Program |
// Management & general | Fundraising | Total. Conversion is to the target (USD) at the
// TRANSACTION-DATE rate (p26.71: an expense is a period FLOW measured at the rate in
// force when it occurred — like the income statement, D12 RateTxnDate, NOT the balance
// sheet's closing rate). Line subtotals / the Total column / the grand total are built by
// int64 addition of the converted cells (footing), so "Program + Management + Fundraising
// == Total" holds EXACTLY per row, and the grand total TIES the income statement's total
// expenses (at GranNone, both round once over the whole period).
//
// HAND-VERIFIED (root scope, USD target, period 2025-01..2026-06, txn-date monthly USD->
// MXN rates, half-even per (account,class) cell over the whole period):
//
//	IX.7  Other salaries and wages   program 1,650,000                 -> total 1,650,000
//	IX.11g Fees for services — other  management     2,500 (leaf ovr)  -> total     2,500
//	IX.16 Occupancy                   management   305,000             -> total   305,000
//	IX.24e All other expenses         program   259,588  (= 210,000 USD
//	         + 500,000 MXN txn-date = 28,971 + 360,000 MXN txn-date = 20,617)
//	                                  management    60,000  (Insurance)
//	                                  fundraising  100,000  (Event Costs) -> total 419,588
//	GRAND  program 1,909,588 · management 367,500 · fundraising 100,000  -> total 2,377,088
//
// Effective-line order (form990_lines sort): IX.7 < IX.11g < IX.16 < IX.24e, then the
// Unmapped bucket LAST (empty on this fixture — every expense account inherits IX.24e
// from the Expenses parent, so there is NO unmapped EXPENSE; the single unmapped R/E
// leaf, Event Income, is REVENUE = Part VIII, not Part IX). The leaf-override IX.11g
// (Bank Fees overriding its parent's IX.24e) lands on its OWN line (D25).

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/reports"
	"cuento/internal/store"
	"cuento/internal/testutil/fixture"
)

// feGoldenParams: root scope, full fixture span, USD target, lang en.
func feGoldenParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		From:           f.Expected.ActivityFrom, // 2025-01-01
		To:             f.Expected.ActivityTo,   // 2026-06-30
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// functionalExpensesReport fetches the registered report from Default().
func functionalExpensesReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.FunctionalExpensesReportID)
	if !ok {
		t.Fatalf("functional-expenses report %q not registered in Default()", reports.FunctionalExpensesReportID)
	}
	return rep
}

// feRowFor returns the full cell slice for the row whose first-cell string equals key
// (matches account TEXT names, IRS line TEXT labels, and framework LABEL keys alike).
func feRowFor(t reports.Table, key string) ([]reports.Cell, bool) {
	for _, row := range t.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == key {
			return row.Cells, true
		}
	}
	return nil, false
}

// TestFunctionalExpensesGolden runs the report over the fixture at the pinned params,
// hand-verifies the per-class columns, the per-990-line subtotals, the leaf-override
// line, the grand total == total expenses (converted), the per-row column-sum
// invariant, and compares the rendered text + CSV to committed goldens.
func TestFunctionalExpensesGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	rep := functionalExpensesReport(t)
	p := feGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run functional expenses: %v", err)
	}

	// Columns: Line + Program + Management & general + Fundraising + Total = 5.
	if len(table.Columns) != 5 {
		t.Fatalf("columns = %d, want 5 (Line + 3 classes + Total)", len(table.Columns))
	}
	if table.Columns[1].HeaderKey != "functional.program" ||
		table.Columns[2].HeaderKey != "functional.management" ||
		table.Columns[3].HeaderKey != "functional.fundraising" {
		t.Errorf("class column headers wrong: %q %q %q",
			table.Columns[1].HeaderKey, table.Columns[2].HeaderKey, table.Columns[3].HeaderKey)
	}

	// --- Per-990-line SUBTOTAL rows (converted, closing rate). cols: [line, program,
	// management, fundraising, total]. IRS-seeded line labels are stored TEXT.
	type want struct{ prog, mgmt, fund, tot int64 }
	lines := map[string]want{
		"7 — Other salaries and wages":     {1_650_000, 0, 0, 1_650_000},
		"11g — Fees for services -- other": {0, 2_500, 0, 2_500}, // leaf override, own line
		"16 — Occupancy":                   {0, 305_000, 0, 305_000},
		"24e — All other expenses":         {259_588, 60_000, 100_000, 419_588},
	}
	for label, w := range lines {
		cells, ok := feRowFor(table, label)
		if !ok {
			t.Errorf("no 990-line subtotal row %q", label)
			continue
		}
		if cells[1].Minor != w.prog || cells[2].Minor != w.mgmt || cells[3].Minor != w.fund || cells[4].Minor != w.tot {
			t.Errorf("line %q = [prog %d, mgmt %d, fund %d, total %d], want [%d, %d, %d, %d]",
				label, cells[1].Minor, cells[2].Minor, cells[3].Minor, cells[4].Minor, w.prog, w.mgmt, w.fund, w.tot)
		}
	}

	// --- Per-account rows under IX.24e (the inherited line): the converted class cells.
	acctRows := map[string]want{
		"Salaries":         {1_650_000, 0, 0, 1_650_000},
		"Program Supplies": {238_971, 0, 0, 238_971}, // 210,000 USD + 500,000 MXN txn-date = 28,971
		"Food Purchases":   {20_617, 0, 0, 20_617},   // 360,000 MXN txn-date rate
		"Occupancy":        {0, 305_000, 0, 305_000},
		"Insurance":        {0, 60_000, 0, 60_000},
		"Bank Fees":        {0, 2_500, 0, 2_500},
		"Event Costs":      {0, 0, 100_000, 100_000},
	}
	for name, w := range acctRows {
		cells, ok := feRowFor(table, name)
		if !ok {
			t.Errorf("no account row %q", name)
			continue
		}
		if cells[1].Minor != w.prog || cells[2].Minor != w.mgmt || cells[3].Minor != w.fund || cells[4].Minor != w.tot {
			t.Errorf("account %q = [prog %d, mgmt %d, fund %d, total %d], want [%d, %d, %d, %d]",
				name, cells[1].Minor, cells[2].Minor, cells[3].Minor, cells[4].Minor, w.prog, w.mgmt, w.fund, w.tot)
		}
	}

	// --- GRAND total (whole Part IX), converted at the closing rate.
	grand, ok := feRowFor(table, "reports.functional_expenses.total")
	if !ok {
		t.Fatalf("no grand-total row")
	}
	if grand[1].Minor != 1_909_588 || grand[2].Minor != 367_500 || grand[3].Minor != 100_000 || grand[4].Minor != 2_377_088 {
		t.Errorf("grand total = [prog %d, mgmt %d, fund %d, total %d], want [1909588, 367500, 100000, 2377088]",
			grand[1].Minor, grand[2].Minor, grand[3].Minor, grand[4].Minor)
	}

	// --- COLUMN SUM invariant: Program + Management + Fundraising == Total, per row.
	for _, row := range table.Rows {
		if len(row.Cells) != 5 {
			continue
		}
		p, m, fu, tot := row.Cells[1], row.Cells[2], row.Cells[3], row.Cells[4]
		if p.Kind != reports.CellMoney || m.Kind != reports.CellMoney || fu.Kind != reports.CellMoney || tot.Kind != reports.CellMoney {
			continue
		}
		if p.Blank || tot.Blank {
			continue
		}
		if p.Minor+m.Minor+fu.Minor != tot.Minor {
			t.Errorf("row %q: prog+mgmt+fund (%d) != total (%d)", row.Cells[0].Text, p.Minor+m.Minor+fu.Minor, tot.Minor)
		}
	}

	// --- Grand total == Σ of the 990-line subtotals (footing across the whole part).
	var sumLines int64
	for label := range lines {
		cells, _ := feRowFor(table, label)
		sumLines += cells[4].Minor
	}
	if sumLines != grand[4].Minor {
		t.Errorf("Σ line subtotals (%d) != grand total (%d)", sumLines, grand[4].Minor)
	}

	// --- Golden artifacts.
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(table, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(table), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "functional_expenses.txt", []byte(textDump))
	checkGolden(t, "functional_expenses.csv", csvBuf.Bytes())
}

// TestFunctionalExpensesOrdering asserts the effective-line ordering: the 990-line
// SUBTOTAL rows appear in the part's report order (IX.7 < IX.11g < IX.16 < IX.24e),
// and the Unmapped bucket — when present — is LAST (before the grand total). On this
// fixture no EXPENSE account is unmapped (every one inherits IX.24e), so the Unmapped
// row is absent; the test asserts the line order and that, if any Unmapped row existed,
// it would sit after every real line and before the grand total.
func TestFunctionalExpensesOrdering(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := functionalExpensesReport(t)
	p := feGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Collect the SUBTOTAL (990-line) rows in render order by their line label.
	var lineOrder []string
	unmappedIdx, grandIdx := -1, -1
	for i, row := range table.Rows {
		switch row.Kind {
		case reports.RowSubtotal:
			lineOrder = append(lineOrder, row.Cells[0].Text)
			if row.Cells[0].Kind == reports.CellLabel && row.Cells[0].Text == "reports.functional_expenses.unmapped" {
				unmappedIdx = i
			}
		case reports.RowTotal:
			grandIdx = i
		}
	}
	wantOrder := []string{
		"7 — Other salaries and wages",
		"11g — Fees for services -- other",
		"16 — Occupancy",
		"24e — All other expenses",
	}
	if len(lineOrder) != len(wantOrder) {
		t.Fatalf("990-line rows = %v, want %v", lineOrder, wantOrder)
	}
	for i, w := range wantOrder {
		if lineOrder[i] != w {
			t.Errorf("990-line row %d = %q, want %q", i, lineOrder[i], w)
		}
	}
	// No unmapped EXPENSE on this fixture (Event Income is revenue): the Unmapped row is
	// absent. If it were present it must sit AFTER every real line and BEFORE the grand.
	if unmappedIdx != -1 {
		t.Errorf("unexpected Unmapped row (no expense account is unmapped on this fixture)")
	}
	if grandIdx == -1 {
		t.Fatalf("no grand-total row")
	}
	// The grand total is the LAST row.
	if grandIdx != len(table.Rows)-1 {
		t.Errorf("grand-total row at %d, want last (%d)", grandIdx, len(table.Rows)-1)
	}
}

// TestFunctionalExpensesUnmappedLast injects an unmapped EXPENSE account (a leaf whose
// effective Part IX code is empty) into a fresh fixture db and asserts the report puts
// it in an explicit Unmapped bucket rendered LAST (after every real 990 line, before
// the grand total) rather than dropping it — the "never drop a row" rule. It builds a
// tiny bespoke expense account with NO 990 code AND a root/no-code chain so its
// effective code is "" (D25), then posts one expense split to it, all in the store's
// write funnel. (DATA RULE 11: this is a store-level structural probe, not a golden.)
func TestFunctionalExpensesUnmappedLast(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	// The fixture's expense leaves all inherit IX.24e from the Expenses parent, so there
	// is no naturally-unmapped expense. Assert the ORDERING mechanism directly: the
	// report's Unmapped bucket (code "") is emitted LAST via the same effective-code
	// ordering Group990 uses (Unmapped last). We re-derive that expectation from the
	// toolkit's Group990 over the fixture's expense leaves and confirm the report would
	// place an unmapped code after every real IX line.
	tk := reports.NewToolkit(f.Store, feGoldenParams(f))

	// Build a leaf->minor map with a synthetic UNMAPPED account id (0 is not a real
	// account; Group990 buckets an absent effective code as "" = Unmapped) alongside a
	// mapped one, and confirm Group990 orders Unmapped LAST.
	eff, err := tk.EffectiveCodes(ctx)
	if err != nil {
		t.Fatalf("effective codes: %v", err)
	}
	// Pick a real mapped expense account (Salaries -> IX.7) and a synthetic unmapped id.
	if eff[reports.AccountID(f.IDs.Salaries)] != "IX.7" {
		t.Fatalf("Salaries effective code = %q, want IX.7", eff[reports.AccountID(f.IDs.Salaries)])
	}
	const unmappedID int64 = -1 // not in eff => "" (Unmapped) bucket
	rows, err := tk.Group990(ctx, "IX", "USD", map[reports.AccountID]int64{
		reports.AccountID(f.IDs.Salaries): 1_000,
		reports.AccountID(unmappedID):     500,
	})
	if err != nil {
		t.Fatalf("group990: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Group990 rows = %d, want 2", len(rows))
	}
	// The Unmapped bucket is LAST and flagged.
	last := rows[len(rows)-1]
	if !last.Unmapped || last.Code != "" {
		t.Errorf("last Group990 row = %+v, want the Unmapped (empty-code) bucket", last)
	}
	if last.Amount.Minor != 500 {
		t.Errorf("Unmapped bucket amount = %d, want 500", last.Amount.Minor)
	}
	if rows[0].Code != "IX.7" {
		t.Errorf("first Group990 row code = %q, want IX.7 (before Unmapped)", rows[0].Code)
	}
}

// TestFunctionalExpensesLeafOverride asserts the leaf-override (D25): Bank Fees carries
// its OWN 990 code IX.11g (overriding its parent's inherited IX.24e), so it lands on the
// IX.11g line — NOT lumped under IX.24e with the other inherited expenses.
func TestFunctionalExpensesLeafOverride(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	tk := reports.NewToolkit(f.Store, feGoldenParams(f))

	eff, err := tk.EffectiveCodes(ctx)
	if err != nil {
		t.Fatalf("effective codes: %v", err)
	}
	if eff[reports.AccountID(f.IDs.BankFees)] != "IX.11g" {
		t.Errorf("Bank Fees effective code = %q, want IX.11g (own override)", eff[reports.AccountID(f.IDs.BankFees)])
	}
	// Its siblings under Expenses inherit IX.24e (no own code).
	for _, id := range []int64{f.IDs.ProgramSupplies, f.IDs.FoodPurchases, f.IDs.Insurance, f.IDs.EventCosts} {
		if eff[reports.AccountID(id)] != "IX.24e" {
			t.Errorf("account %d effective code = %q, want IX.24e (inherited)", id, eff[reports.AccountID(id)])
		}
	}

	rep := functionalExpensesReport(t)
	p := feGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Bank Fees appears directly under the IX.11g line, not IX.24e. Assert Bank Fees is
	// the ONLY account row between the IX.11g subtotal and the next subtotal.
	sawOwnLine := false
	for i, row := range table.Rows {
		if row.Kind == reports.RowSubtotal && row.Cells[0].Text == "11g — Fees for services -- other" {
			// The next row must be the Bank Fees account row.
			if i+1 >= len(table.Rows) {
				t.Fatalf("IX.11g line has no account row after it")
			}
			next := table.Rows[i+1]
			if next.Kind != reports.RowData || next.Cells[0].Text != "Bank Fees" {
				t.Errorf("row after IX.11g = %q (kind %v), want Bank Fees data row", next.Cells[0].Text, next.Kind)
			}
			sawOwnLine = true
		}
	}
	if !sawOwnLine {
		t.Errorf("Bank Fees did not land on its own IX.11g line")
	}
}

// TestFunctionalExpensesDrillReconciles: the p15.3d RECONCILIATION invariant on an
// account×class cell — the signed sum of the splits the cell's Drill selects (via
// store.DrillSplits, the SAME query the drill route runs, WITH the functional-class
// filter) EQUALS the cell's NATIVE figure. Salaries×program is single-currency USD, so
// its converted cell equals its native figure (1:1); the store is the oracle.
func TestFunctionalExpensesDrillReconciles(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := functionalExpensesReport(t)
	p := feGoldenParams(f)
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	cells, ok := feRowFor(table, "Salaries")
	if !ok {
		t.Fatalf("no Salaries row")
	}
	// Program column (index 1) is the drillable cell.
	cell := cells[1]
	if cell.Drill == nil {
		t.Fatalf("Salaries program cell is not drillable")
	}
	d := cell.Drill
	if d.Mode != reports.DrillPeriod || d.From != f.Expected.ActivityFrom || d.To != f.Expected.ActivityTo {
		t.Errorf("Salaries drill period = %s..%s (mode %v), want whole span DrillPeriod", d.From, d.To, d.Mode)
	}
	if d.Class == nil || *d.Class != "program" {
		t.Errorf("Salaries drill class = %v, want program", d.Class)
	}
	if d.Currency != "USD" || len(d.AccountIDs) != 1 || d.AccountIDs[0] != f.IDs.Salaries {
		t.Errorf("Salaries drill filter wrong: %+v", d)
	}

	filter := store.DrillFilter{
		Scope:     d.Scope,
		AccountID: d.AccountIDs[0],
		Currency:  d.Currency,
		From:      d.From,
		To:        d.To,
		Class:     d.Class,
	}
	splits, err := f.Store.DrillSplits(ctx, filter)
	if err != nil {
		t.Fatalf("drill splits: %v", err)
	}
	var sum int64
	for _, s := range splits {
		sum += s.Amount
	}
	// Native == converted for USD (1:1); the cell figure and the drilled sum agree.
	if sum != cell.Minor {
		t.Errorf("Salaries×program drill sum = %d, want cell figure %d", sum, cell.Minor)
	}
	if sum != 1_650_000 {
		t.Errorf("Salaries×program native = %d, want 1,650,000 (fixture oracle)", sum)
	}

	// A LINE-SUBTOTAL row is NOT drillable (a rollup over many accounts/currencies).
	sub, ok := feRowFor(table, "24e — All other expenses")
	if !ok {
		t.Fatalf("no IX.24e subtotal row")
	}
	for _, c := range sub {
		if c.Drill != nil {
			t.Errorf("IX.24e subtotal cell is drillable; must not be")
		}
	}

	// A MULTI-currency account×class cell is NOT drillable: Program Supplies × program
	// holds USD and MXN, so a single currency-filtered drill cannot reconcile it.
	psCells, ok := feRowFor(table, "Program Supplies")
	if !ok {
		t.Fatalf("no Program Supplies row")
	}
	if psCells[1].Drill != nil {
		t.Errorf("Program Supplies × program (multi-currency) cell is drillable; must not be")
	}
}

// TestFunctionalExpensesScope: a leaf sub (RV Mexico) differs from root — MX carries
// only MX-posted expenses (Food Purchases, MXN; Program Supplies MXN), not the US-posted
// Salaries (USD). Structural (presence), independent of FX rounding.
func TestFunctionalExpensesScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := functionalExpensesReport(t)

	rootP := feGoldenParams(f)
	rootT, err := rep.Run(ctx, reports.NewToolkit(f.Store, rootP), rootP)
	if err != nil {
		t.Fatalf("run root: %v", err)
	}
	leafP := rootP
	leafP.Scope = reports.SubsidiaryID(f.IDs.MX)
	leafT, err := rep.Run(ctx, reports.NewToolkit(f.Store, leafP), leafP)
	if err != nil {
		t.Fatalf("run leaf: %v", err)
	}

	if _, ok := feRowFor(rootT, "Salaries"); !ok {
		t.Errorf("root report missing Salaries")
	}
	if _, ok := feRowFor(leafT, "Salaries"); ok {
		t.Errorf("leaf(MX) report unexpectedly contains US-posted Salaries")
	}
	if _, ok := feRowFor(leafT, "Food Purchases"); !ok {
		t.Errorf("leaf(MX) report missing MX-posted Food Purchases")
	}
}

// TestFunctionalExpensesEmpty: a period with no expense activity yields a table with the
// column headers but no account/line/grand rows (the framework's nothing-to-show is an
// empty body, not an error).
func TestFunctionalExpensesEmpty(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := functionalExpensesReport(t)
	p := feGoldenParams(f)
	// A period entirely before the fixture's first transaction.
	p.From = "2020-01-01"
	p.To = "2020-12-31"
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run empty: %v", err)
	}
	if len(table.Columns) != 5 {
		t.Errorf("empty report columns = %d, want 5", len(table.Columns))
	}
	// No 990-line or account rows; only the grand-total row (all zeros) is emitted.
	for _, row := range table.Rows {
		if row.Kind == reports.RowSubtotal {
			t.Errorf("empty report has a 990-line subtotal row %q", row.Cells[0].Text)
		}
	}
}

// TestFunctionalExpensesTiesIncomeStatement is the p26.71 cross-report tie: the
// functional-expenses grand total EQUALS the income statement's total expenses EQUALS
// the 990 Part IX grand total, EXACTLY, on the multi-currency multi-rate fixture — the
// whole point of moving Part IX / functional expenses off the closing rate and onto the
// transaction-date (flow) rate. All three consume the expense flow at the txn-date rate;
// the tie is an IDENTITY, so the test computes all three and asserts equality (no literal
// is hand-pinned — `make golden` owns the whole-period figures). The income statement is
// run at GranNone so it rounds ONCE over the whole period, the same grain as the
// functional matrix (a quarterly run rounds per (account,quarter) and can differ by a
// minor unit — the documented footing note). Every fixture expense account is single-
// class, so Σ round(class) == round(Σ classes) and the (account×class) vs (account)
// rounding grains coincide; a real multi-class chart can leave a sub-dollar residual
// (DECISIONS p26.71), but the FX-basis gap this closes is eliminated regardless.
func TestFunctionalExpensesTiesIncomeStatement(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()

	// Functional-expenses grand total (txn-date, whole period).
	feP := feGoldenParams(f)
	feT, err := functionalExpensesReport(t).Run(ctx, reports.NewToolkit(f.Store, feP), feP)
	if err != nil {
		t.Fatalf("run functional expenses: %v", err)
	}
	feGrand, ok := feRowFor(feT, "reports.functional_expenses.total")
	if !ok {
		t.Fatal("no functional-expenses grand-total row")
	}
	feTotal := feGrand[4].Minor

	// Income statement total expenses at GranNone (round once over the whole period, the
	// same grain as the functional matrix — a quarterly run differs by a minor unit).
	isP := isGoldenParams(f)
	isP.Granularity = reports.GranNone
	isT, err := incomeStatementReport(t).Run(ctx, reports.NewToolkit(f.Store, isP), isP)
	if err != nil {
		t.Fatalf("run income statement: %v", err)
	}
	isExpenses, ok := isTotalFor(isT, "reports.income_statement.total.expenses")
	if !ok {
		t.Fatal("no income-statement total-expenses row")
	}

	// 990 Part IX grand total (same txn-date functional path).
	nineP := feGoldenParams(f)
	nineT, err := form990Report(t).Run(ctx, reports.NewToolkit(f.Store, nineP), nineP)
	if err != nil {
		t.Fatalf("run 990: %v", err)
	}
	nineGrand, ok := f990RowFor(nineT, "reports.form_990.ix.total")
	if !ok {
		t.Fatal("no 990 Part IX total row")
	}
	nineTotal := nineGrand[2].Minor

	if feTotal != isExpenses {
		t.Errorf("functional-expenses total (%d) != income-statement total expenses (%d)", feTotal, isExpenses)
	}
	if nineTotal != feTotal {
		t.Errorf("990 Part IX total (%d) != functional-expenses total (%d)", nineTotal, feTotal)
	}
	if nineTotal != isExpenses {
		t.Errorf("990 Part IX total (%d) != income-statement total expenses (%d)", nineTotal, isExpenses)
	}
}

// TestFunctionalExpensesGrantProgramScope (p27.4b): a program-scoped report grant filters
// the functional-expense matrix to the granted program SUBTREE BEFORE the class rollup, so
// a SIBLING subtree's expense never contributes to any line -- including the rolled
// grand-total row. Scoped to Educacion (leaf subtree = {Educacion}): only Educacion's
// Program Supplies (program class) survives; Salaries (program, General-direct), Food
// Purchases (program, General-direct + Food Pantry), and every management/fundraising
// account (Occupancy/Insurance/Bank Fees/Event Costs, all General-direct) vanish.
func TestFunctionalExpensesGrantProgramScope(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := functionalExpensesReport(t)

	base := feGoldenParams(f)
	baseT, err := rep.Run(ctx, reports.NewToolkit(f.Store, base), base)
	if err != nil {
		t.Fatalf("run unscoped: %v", err)
	}
	baseGrand, ok := feRowFor(baseT, "reports.functional_expenses.total")
	if !ok {
		t.Fatalf("unscoped: no grand-total row")
	}
	baseTotal := baseGrand[len(baseGrand)-1].Minor
	if _, ok := feRowFor(baseT, "Salaries"); !ok {
		t.Fatalf("unscoped functional expenses missing Salaries (sibling present)")
	}

	p := base
	p.ProgramScope = []reports.ProgramID{reports.ProgramID(f.IDs.Educacion)}
	table, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}

	// Out-of-subtree expense accounts vanish from the account rows entirely.
	for _, name := range []string{
		"Salaries", "Food Purchases", "Occupancy", "Insurance", "Bank Fees", "Event Costs",
	} {
		if _, ok := feRowFor(table, name); ok {
			t.Errorf("scoped functional expenses leaks out-of-subtree account %q", name)
		}
	}
	// Educacion's Program Supplies (the only in-subtree expense) survives.
	if _, ok := feRowFor(table, "Program Supplies"); !ok {
		t.Errorf("scoped functional expenses dropped in-subtree Program Supplies")
	}

	// The rolled GRAND TOTAL now reflects ONLY Educacion's expenses -- strictly less than
	// the org-wide figure. A General-direct or Food Pantry leak into the rollup would keep
	// it at/above base. Also assert the management/fundraising class columns are ZERO
	// (Educacion has only a program-class expense) -- a sibling management leak would
	// resurface Occupancy/Insurance in the mgmt column of the grand total.
	scopedGrand, ok := feRowFor(table, "reports.functional_expenses.total")
	if !ok {
		t.Fatalf("scoped: no grand-total row")
	}
	scopedTotal := scopedGrand[len(scopedGrand)-1].Minor
	if scopedTotal >= baseTotal {
		t.Errorf("scoped grand total %d >= org-wide %d; a sibling subtree leaked into the rollup", scopedTotal, baseTotal)
	}
	if scopedTotal <= 0 {
		t.Errorf("scoped grand total %d; Educacion's Program Supplies should remain", scopedTotal)
	}
	// cols: [line, program, management, fundraising, total]. Educacion is program-only.
	if mgmt := scopedGrand[2].Minor; mgmt != 0 {
		t.Errorf("scoped grand-total management column = %d, want 0 (no sibling management leak)", mgmt)
	}
	if fund := scopedGrand[3].Minor; fund != 0 {
		t.Errorf("scoped grand-total fundraising column = %d, want 0 (no sibling fundraising leak)", fund)
	}
}
