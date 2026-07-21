package reports_test

// p15.10 / p31-10a program statement report tests. The report is an ACCOUNT-row ×
// (functional-class / program)-COLUMN matrix, single currency (converted to the target at
// the transaction-date rate). Columns left→right: Account | Total | Admin | Fundraising |
// Program services → [program tree]. Rows: a Revenue section and an Expenses section (the
// chart hierarchy, placeholder parents as roll-up subtotals), closing with a Net line.
//
// COLUMN PLACEMENT RULES (the confirmed design, asserted below):
//   - a management expense → Admin + Total ONLY (blank in Fundraising and every program col);
//   - a program expense → its program column + General (rolled) + Total;
//   - revenue → its program column + General + Total, BLANK in Admin/Fundraising.
//
// Numbers are tied CROSS-REPORT to the toolkit's FunctionalMatrix (the functional-expenses
// source) and Activity (the income statement source), both at the same RateTxnDate grain, so
// the matrix reconciles per account exactly rather than against hand-copied converted values.

import (
	"bytes"
	"context"
	"testing"

	"cuento/internal/ids"
	"cuento/internal/reports"
	"cuento/internal/testutil/fixture"
)

// psReport fetches the registered program statement from Default().
func psReport(t *testing.T) reports.Report {
	t.Helper()
	rep, ok := reports.Default().Get(reports.ProgramStatementReportID)
	if !ok {
		t.Fatalf("program statement report %q not registered in Default()", reports.ProgramStatementReportID)
	}
	return rep
}

// psParams runs the statement over the whole fixture span, root scope, lang en, converted to
// USD (the matrix is single-currency now, so a target is mandatory).
func psParams(f *fixture.Fixture) reports.Params {
	return reports.Params{
		Scope:          reports.SubsidiaryID(f.IDs.Root),
		From:           f.Expected.ActivityFrom, // 2025-01-01
		To:             f.Expected.AsOf,         // 2026-06-30
		TargetCurrency: "USD",
		Lang:           "en",
	}
}

// psColIndex returns the index of the money column whose header key OR verbatim text matches,
// or -1. Program columns carry a verbatim HeaderText (the program name); the fixed columns
// carry a HeaderKey.
func psColIndex(tbl reports.Table, keyOrName string) int {
	for i, c := range tbl.Columns {
		if c.HeaderKey == keyOrName || c.HeaderText == keyOrName {
			return i
		}
	}
	return -1
}

// psRow returns the DATA/subtotal row whose first cell text is name, or (Row{}, false).
func psRow(tbl reports.Table, name string) (reports.Row, bool) {
	for _, row := range tbl.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == name {
			return row, true
		}
	}
	return reports.Row{}, false
}

// psCell returns the (minor, blank, found) of the row named `name` at money column `col`.
func psCell(tbl reports.Table, name string, col int) (int64, bool, bool) {
	row, ok := psRow(tbl, name)
	if !ok || col < 0 || col >= len(row.Cells) {
		return 0, false, false
	}
	c := row.Cells[col]
	return c.Minor, c.Blank, true
}

// TestProgramStatementColumns asserts the matrix column structure: Account, then Total, Admin,
// Fundraising, then the program tree (General, Educacion, Food Pantry) under the Program-
// services stacked group. No Currency column (single-currency, converted).
func TestProgramStatementColumns(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if tbl.Columns[0].HeaderKey != "reports.program_statement.col.account" {
		t.Errorf("col 0 = %q, want the account column", tbl.Columns[0].HeaderKey)
	}
	for i, want := range []string{
		"reports.program_statement.col.total",
		"reports.program_statement.col.admin",
		"functional.fundraising",
	} {
		if tbl.Columns[1+i].HeaderKey != want {
			t.Errorf("col %d = %q, want %q", 1+i, tbl.Columns[1+i].HeaderKey, want)
		}
	}
	// Program columns follow, each a verbatim program name under the program-services group.
	for _, prog := range []string{"General", "Educacion", "Food Pantry"} {
		ci := psColIndex(tbl, prog)
		if ci < 0 {
			t.Fatalf("program column %q missing", prog)
		}
		c := tbl.Columns[ci]
		if c.HeaderText != prog {
			t.Errorf("program column %d HeaderText = %q, want %q (verbatim)", ci, c.HeaderText, prog)
		}
		if c.Group == nil || c.Group.Key != "reports.program_statement.group.program_services" {
			t.Errorf("program column %q not under the program-services group", prog)
		}
	}
	// No column carries the dropped Currency header.
	if psColIndex(tbl, "reports.program_statement.col.currency") >= 0 {
		t.Errorf("statement still has a Currency column; the matrix is single-currency")
	}
}

// TestProgramStatementColumnPlacement is the REQUIRED focused placement test:
//   - a management expense (Occupancy) lands in Admin + Total ONLY (blank elsewhere);
//   - a program expense (Salaries) lands in its program column + General (rolled) + Total,
//     blank in Admin/Fundraising;
//   - a revenue account (Contributions) lands in its program column + General + Total, BLANK
//     in Admin and Fundraising.
func TestProgramStatementColumnPlacement(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	total := psColIndex(tbl, "reports.program_statement.col.total")
	admin := psColIndex(tbl, "reports.program_statement.col.admin")
	fundr := psColIndex(tbl, "functional.fundraising")
	general := psColIndex(tbl, "General")
	educ := psColIndex(tbl, "Educacion")
	if total < 0 || admin < 0 || fundr < 0 || general < 0 || educ < 0 {
		t.Fatalf("a required column is missing (total=%d admin=%d fundr=%d general=%d educ=%d)", total, admin, fundr, general, educ)
	}

	// blank asserts a cell is present but BLANK (no amount); nonblank asserts an amount.
	blank := func(name string, col int, label string) {
		_, isBlank, ok := psCell(tbl, name, col)
		if !ok {
			t.Fatalf("%s: no cell at %s column", name, label)
		}
		if !isBlank {
			t.Errorf("%s %s cell should be BLANK", name, label)
		}
	}
	nonblank := func(name string, col int, label string) int64 {
		m, isBlank, ok := psCell(tbl, name, col)
		if !ok || isBlank {
			t.Errorf("%s %s cell should carry an amount (blank=%v ok=%v)", name, label, isBlank, ok)
		}
		return m
	}

	// Management expense (Occupancy): Admin + Total only.
	nonblank("Occupancy", admin, "Admin")
	nonblank("Occupancy", total, "Total")
	blank("Occupancy", fundr, "Fundraising")
	blank("Occupancy", general, "General(program)")
	// Occupancy's Total == its Admin (it has no other class/program activity).
	if got := nonblank("Occupancy", total, "Total"); got != nonblank("Occupancy", admin, "Admin") {
		t.Errorf("Occupancy Total %d != Admin (management-only expense)", got)
	}

	// Program expense (Salaries): its program column (General-direct → General) + Total.
	nonblank("Salaries", general, "General(program)")
	nonblank("Salaries", total, "Total")
	blank("Salaries", admin, "Admin")
	blank("Salaries", fundr, "Fundraising")

	// Revenue (Contributions): program column (General) + Total; BLANK in Admin & Fundraising.
	nonblank("Contributions", general, "General(program)")
	nonblank("Contributions", total, "Total")
	blank("Contributions", admin, "Admin")
	blank("Contributions", fundr, "Fundraising")
}

// TestProgramStatementTiesFunctionalMatrix ties the EXPENSE columns to the toolkit's
// FunctionalMatrix (the functional-expenses source) at the same RateTxnDate grain, per
// account, exactly: Admin == class management, Fundraising == class fundraising, and General
// (rolled program column, root) == class program (org-wide, since General is the root and
// folds every program). This is the strongest per-account correctness check.
func TestProgramStatementTiesFunctionalMatrix(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tk := reports.NewToolkit(f.Store, p)
	tbl, err := rep.Run(ctx, tk, p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	fm, err := tk.FunctionalMatrix(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		p.From, p.To, reports.ConvertOpts{To: "USD", Mode: reports.RateTxnDate})
	if err != nil {
		t.Fatalf("functional matrix: %v", err)
	}
	names := psAccountNames(t, f)
	admin := psColIndex(tbl, "reports.program_statement.col.admin")
	fundr := psColIndex(tbl, "functional.fundraising")
	general := psColIndex(tbl, "General")

	classMinor := func(acct ids.AccountID, cl reports.Class) int64 {
		var m int64
		for _, a := range fm[reports.AccountID(acct)][cl] {
			if a.Currency == "USD" {
				m += a.Minor
			}
		}
		return m
	}
	for acct := range fm {
		name := names[ids.AccountID(acct)]
		mgmt := classMinor(ids.AccountID(acct), "management")
		fund := classMinor(ids.AccountID(acct), "fundraising")
		prog := classMinor(ids.AccountID(acct), "program")

		if mgmt != 0 {
			if got, _, _ := psCell(tbl, name, admin); got != mgmt {
				t.Errorf("%s Admin = %d, want management %d", name, got, mgmt)
			}
		}
		if fund != 0 {
			if got, _, _ := psCell(tbl, name, fundr); got != fund {
				t.Errorf("%s Fundraising = %d, want fundraising %d", name, got, fund)
			}
		}
		// General is the root program column, so its program-services value for an expense
		// account == that account's org-wide class=program total (expenses shown +).
		if prog != 0 {
			if got, _, _ := psCell(tbl, name, general); got != prog {
				t.Errorf("%s General(program) = %d, want class=program %d", name, got, prog)
			}
		}
	}

	// Revenue tie: revenue carries no functional class, so the functional matrix can't check
	// it. Tie the General-root revenue column to the toolkit Activity (the income-statement
	// source) at the same RateTxnDate grain — each revenue account's General cell (shown +) ==
	// −(its org-wide converted net-debit), since General folds every program's revenue.
	act, err := tk.Activity(ctx, reports.Scope{Sub: reports.SubsidiaryID(f.IDs.Root)},
		p.From, p.To, reports.ConvertOpts{To: "USD", Mode: reports.RateTxnDate})
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	types := psAccountTypes(t, f)
	for acct, amts := range act {
		if types[ids.AccountID(acct)] != "revenue" {
			continue
		}
		var raw int64
		for _, a := range amts {
			if a.Currency == "USD" {
				raw += a.Minor
			}
		}
		if raw == 0 {
			continue
		}
		want := -raw // revenue is a credit (negative net-debit) shown positive
		if got, _, _ := psCell(tbl, names[ids.AccountID(acct)], general); got != want {
			t.Errorf("%s General(revenue) = %d, want −activity %d", names[ids.AccountID(acct)], got, want)
		}
	}
}

// TestProgramStatementTotalIdentity: for every account row, Total == Admin + Fundraising +
// Σ(ROOT program columns). With a single root (General) that is Admin + Fundraising + General.
// Summing every program column instead would double-count General's rolled-in descendants —
// this asserts the report sums only disjoint roots.
func TestProgramStatementTotalIdentity(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	total := psColIndex(tbl, "reports.program_statement.col.total")
	admin := psColIndex(tbl, "reports.program_statement.col.admin")
	fundr := psColIndex(tbl, "functional.fundraising")
	general := psColIndex(tbl, "General")

	amt := func(cells []reports.Cell, col int) int64 {
		if col < 0 || col >= len(cells) {
			return 0
		}
		return cells[col].Minor // a blank cell has Minor 0
	}
	checked := 0
	for _, row := range tbl.Rows {
		if len(row.Cells) == 0 || row.Cells[0].Text == "" {
			continue // section-label rows
		}
		want := amt(row.Cells, admin) + amt(row.Cells, fundr) + amt(row.Cells, general)
		if got := amt(row.Cells, total); got != want {
			t.Errorf("row %q Total = %d, want Admin+FR+General %d", row.Cells[0].Text, got, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no rows checked")
	}
}

// TestProgramStatementProgramRollup: a parent program column (General) folds in its
// descendants. Educacion's Program Supplies (program-class) must appear in BOTH the Educacion
// column and the General column, and General's value >= Educacion's (it also folds General-
// direct + Food Pantry).
func TestProgramStatementProgramRollup(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	general := psColIndex(tbl, "General")
	educ := psColIndex(tbl, "Educacion")

	genPS, genBlank, _ := psCell(tbl, "Program Supplies", general)
	educPS, educBlank, _ := psCell(tbl, "Program Supplies", educ)
	if genBlank || educBlank {
		t.Fatalf("Program Supplies blank in a program column (general blank=%v, educ blank=%v)", genBlank, educBlank)
	}
	if educPS <= 0 {
		t.Errorf("Educacion Program Supplies = %d, want > 0", educPS)
	}
	if genPS < educPS {
		t.Errorf("General Program Supplies %d < Educacion %d — parent must fold in the child", genPS, educPS)
	}
}

// TestProgramStatementScopedColumns: picking a program scopes the visible program COLUMNS to
// that subtree. Picking Educacion (a leaf) shows only the Educacion program column (no General
// / Food Pantry columns) — and Admin/Fundraising narrow to that subtree too (required for the
// Total identity), so a General-only management expense (Occupancy) drops out.
func TestProgramStatementScopedColumns(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	p.Program = reports.ProgramID(f.IDs.Educacion)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run scoped: %v", err)
	}
	if psColIndex(tbl, "General") >= 0 {
		t.Errorf("scoped-to-Educacion view still shows the General program column")
	}
	if psColIndex(tbl, "Food Pantry") >= 0 {
		t.Errorf("scoped view leaks the sibling Food Pantry column")
	}
	if psColIndex(tbl, "Educacion") < 0 {
		t.Errorf("scoped view is missing the Educacion column")
	}
	// Occupancy (a General-direct management expense) has NO Educacion-subtree activity → the
	// row drops out entirely (its only activity was out of scope).
	if _, ok := psRow(tbl, "Occupancy"); ok {
		t.Errorf("scoped view still shows Occupancy (General-only management expense out of subtree)")
	}
}

// TestProgramStatementHeaderDataAttrs (10b hook): each program column header carries the tree
// data attributes — a program id, a parent program id for a child, and a group marker for a
// program with children. General (root, has children) carries program + program-group;
// Educacion (child leaf) carries program + program-parent (General's id) and NO group marker.
func TestProgramStatementHeaderDataAttrs(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	col := func(name string) reports.Column {
		i := psColIndex(tbl, name)
		if i < 0 {
			t.Fatalf("program column %q missing", name)
		}
		return tbl.Columns[i]
	}
	gen := col("General")
	if gen.Group == nil || gen.Group.Data["program"] == "" {
		t.Errorf("General column missing data-program attr")
	}
	if gen.Group.Data["program-group"] != "1" {
		t.Errorf("General (root with children) missing the group-parent marker")
	}
	if gen.Group.Data["program-parent"] != "" {
		t.Errorf("General is a root; it must not carry a program-parent")
	}
	educ := col("Educacion")
	if educ.Group.Data["program-parent"] == "" {
		t.Errorf("Educacion (child) missing data-program-parent")
	}
	if educ.Group.Data["program-group"] != "" {
		t.Errorf("Educacion (leaf) must not carry the group-parent marker")
	}
}

// TestProgramStatementCollapsibleTree: the account rows form a collapsible tree. The report
// registers Tree: true and a placeholder-parent account (the "Expenses" section parent)
// renders as a roll-up SUBTOTAL over its indented leaves; its Total == the sum of its leaves'
// Totals.
func TestProgramStatementCollapsibleTree(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	if !rep.Tree {
		t.Fatalf("program statement must register Tree: true for collapsibility")
	}
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	total := psColIndex(tbl, "reports.program_statement.col.total")

	// Find the "Expenses" placeholder subtotal row and sum its contiguous deeper leaf rows.
	ei := -1
	for i, row := range tbl.Rows {
		if len(row.Cells) > 0 && row.Cells[0].Text == "Expenses" && row.Kind == reports.RowSubtotal {
			ei = i
			break
		}
	}
	if ei < 0 {
		t.Fatal("Expenses placeholder subtotal row missing")
	}
	exp := tbl.Rows[ei]
	if exp.Cells[total].Drill != nil {
		t.Errorf("Expenses subtotal Total cell is drillable; a rollup must not drill")
	}
	var leafSum int64
	for i := ei + 1; i < len(tbl.Rows); i++ {
		r := tbl.Rows[i]
		if r.Indent <= exp.Indent {
			break // left the Expenses subtree
		}
		if r.Kind == reports.RowData && len(r.Cells) > total && !r.Cells[total].Blank {
			// only leaf data rows (deeper placeholders would double count); leaves are the
			// deepest rows, and the flat R/E tree here has no nested placeholders.
			if r.Indent == exp.Indent+1 {
				leafSum += r.Cells[total].Minor
			}
		}
	}
	if exp.Cells[total].Minor != leafSum {
		t.Errorf("Expenses subtotal Total = %d, want Σ leaf Totals %d", exp.Cells[total].Minor, leafSum)
	}
}

// TestProgramStatementGolden runs the converted statement and compares the rendered text +
// CSV to committed goldens (reviewed via `make golden`).
func TestProgramStatementGolden(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	exps := goldenExps(t, f)
	textDump := reports.DumpTable(tbl, goldenLocalize, exps)
	var csvBuf bytes.Buffer
	if err := reports.WriteCSV(&csvBuf, localizeLabels(tbl), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	checkGolden(t, "program_statement.txt", []byte(textDump))
	checkGolden(t, "program_statement.csv", csvBuf.Bytes())
}

// TestProgramStatementCSVParses: the statement CSV parses to well-formed records with a leaf
// header row (Account, Total, Admin, Fundraising, program names). The stacked group super-
// header is a web-only concern; the CSV emits the flat leaf header.
func TestProgramStatementCSVParses(t *testing.T) {
	f := fixture.New(t)
	f.ExtendRates(t)
	ctx := context.Background()
	rep := psReport(t)
	p := psParams(f)
	tbl, err := rep.Run(ctx, reports.NewToolkit(f.Store, p), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	exps := goldenExps(t, f)
	var buf bytes.Buffer
	if err := reports.WriteCSV(&buf, localizeLabels(tbl), goldenLocalize, exps); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	recs := parseCSV(t, buf.Bytes())
	if len(recs) < 2 {
		t.Fatalf("csv has %d records, want header + rows", len(recs))
	}
	for i, h := range []string{"Account", "Total", "Admin", "Fundraising", "General"} {
		if recs[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, recs[0][i], h)
		}
	}
}

// --- helpers ----------------------------------------------------------------

// psAccountNames returns account id -> resolved (en) name from the store tree.
func psAccountNames(t *testing.T, f *fixture.Fixture) map[ids.AccountID]string {
	t.Helper()
	tree, err := f.Store.Tree(context.Background(), "en", nil)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	m := make(map[ids.AccountID]string, len(tree))
	for _, r := range tree {
		m[r.ID] = r.Name
	}
	return m
}
