package reports

import (
	"context"
	"sort"
	"strconv"
)

// FXDetailReportID is the id (URL slug + registry key) of the FX conversion-details
// report (p31): the auditor-facing ASC 830-20 foreign-currency REMEASUREMENT detail.
// AS OF a date and scoped to a subsidiary it lists every foreign-currency MONETARY,
// NON-intercompany balance and shows exactly how the change in net assets absorbs the
// FX gain/loss: the native residual balance, the closing rate applied, the historical
// (transaction-date) basis it was built at, its value remeasured at the closing rate,
// and the difference recognized in income (negative = a loss). It is a pure read over
// the shared FXRemeasurementAsOf toolkit (fx.go), the same computation that feeds the
// Statement-of-Activities FX gain/loss line, so the report and the statement agree by
// construction.
//
// Rows GROUP BY SUBSIDIARY: a section header names the holding sub and its functional
// currency, one data row per foreign-currency monetary item follows, and a section
// total sums that sub's remeasurement gain/loss (in its functional currency). At the
// end a per-functional-currency TOTAL row states the amount recognized in the
// consolidated change in net assets (ByFunctional). When the scope holds no foreign-
// currency monetary balance the report is not an error -- it emits a single note row.
const FXDetailReportID = "fx_detail"

// registerFXDetail registers the FX conversion-details report (p31) into reg under the
// "financial" group. It offers only the as-of control; the scope selector is always
// shown, and the functional/target currency is a property of each sub (not chosen).
func registerFXDetail(reg *Registry) {
	reg.Register(Report{
		ID:         FXDetailReportID,
		TitleKey:   "reports.fx_detail.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true},
		Run:        runFXDetail,
	})
}

// runFXDetail computes the FX conversion-details Table (p31). It calls the shared
// FXRemeasurementAsOf toolkit (fx.go) -- whose Items are ALREADY filtered to foreign,
// monetary, non-intercompany balances -- resolves account and subsidiary display names
// (in the request language, p.LangOr), and renders the per-subsidiary detail with a
// section total per sub and a grand total per functional currency. It reads nothing
// the toolkit did not already compute; the numbers are the toolkit's.
func runFXDetail(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	fx, err := tk.FXRemeasurementAsOf(ctx, Scope{Sub: p.Scope}, p.AsOf)
	if err != nil {
		return Table{}, err
	}

	t := Table{Columns: fxColumns()}

	// Empty state: no foreign-currency monetary balance in the scope (a legitimate
	// "nothing to show", not an error). One note row spanning the account column.
	if len(fx.Items) == 0 {
		t.Rows = append(t.Rows, Row{
			Cells: fxNoteRow(LabelCell("reports.fx_detail.empty")),
			Kind:  RowData,
		})
		return t, nil
	}

	// Resolve names once. Account names are the fully-qualified per-lang paths (D5),
	// the same hierarchy-path machinery the budget reports use; subsidiary names are
	// stored proper nouns from the sub tree (which also carries each sub's functional
	// base_currency).
	acctPaths, err := tk.Store().AccountPaths(ctx, p.LangOr())
	if err != nil {
		return Table{}, err
	}
	subTree, err := tk.Store().SubTree(ctx)
	if err != nil {
		return Table{}, err
	}
	subName := make(map[SubsidiaryID]string, len(subTree))
	for _, s := range subTree {
		subName[s.ID] = s.Name
	}

	// Group items by subsidiary, preserving the toolkit's deterministic item order for
	// the first appearance of each sub (SubDatedBalancesAsOf orders by sub/account/ccy).
	bySub := map[SubsidiaryID][]FXItem{}
	var subOrder []SubsidiaryID
	for _, it := range fx.Items {
		if _, seen := bySub[it.Sub]; !seen {
			subOrder = append(subOrder, it.Sub)
		}
		bySub[it.Sub] = append(bySub[it.Sub], it)
	}

	for _, sub := range subOrder {
		items := bySub[sub]
		functional := items[0].Functional

		// Section header: the sub name (proper noun) + a localized "functional" label +
		// the functional currency code. Rendered as a multi-cell row because the report
		// carries no localizer (label keys are localized at render time, proper nouns are
		// not), so "RV Estados Unidos -- functional USD" cannot be one interpolated label.
		t.Rows = append(t.Rows, Row{
			Cells: fxHeaderRow(TextCell(subName[sub]), functional),
			Kind:  RowData,
		})

		var sectionTotal int64
		for _, it := range items {
			sectionTotal += it.RemeasureMinor
			t.Rows = append(t.Rows, Row{
				Indent: 1,
				Cells: []Cell{
					TextCell(acctPaths[it.Account]),
					TextCell(it.Currency),
					MoneyCell(it.NativeMinor, it.Currency),
					MoneyCell(it.HistBasisMinor, functional),
					MoneyCell(it.ClosingMinor, functional),
					MoneyCell(it.RemeasureMinor, functional),
					TextCell(fxRate(it.ClosingRate)),
					DateCell(it.ClosingRateDate),
				},
				Kind: RowData,
			})
		}

		// Section total: this sub's Σ remeasurement gain/loss, in its functional currency.
		t.Rows = append(t.Rows, Row{
			Cells: fxTotalRow(
				LabelCell("reports.fx_detail.total.subsidiary"),
				MoneyCell(sectionTotal, functional),
			),
			Kind: RowSectionTotal,
		})
	}

	// Grand total per functional currency: the amount recognized in the consolidated
	// change in net assets (ByFunctional). Sorted for deterministic output.
	funcs := make([]string, 0, len(fx.ByFunctional))
	for ccy := range fx.ByFunctional {
		funcs = append(funcs, ccy)
	}
	sort.Strings(funcs)
	for _, ccy := range funcs {
		t.Rows = append(t.Rows, Row{
			Cells: fxTotalRow(
				LabelCell("reports.fx_detail.total.recognized"),
				MoneyCell(fx.ByFunctional[ccy], ccy),
			),
			Kind: RowTotal,
		})
	}

	return t, nil
}

// fxColumns is the report's fixed 8-column shape: account, foreign currency, native
// balance, historical basis, remeasured at closing, FX gain/(loss), the closing rate,
// and the rate's date (which may be stale, p14.1).
func fxColumns() []Column {
	return []Column{
		{HeaderKey: "reports.fx_detail.col.account", Align: AlignLeft},
		{HeaderKey: "reports.fx_detail.col.currency", Align: AlignLeft},
		{HeaderKey: "reports.fx_detail.col.native", Align: AlignRight},
		{HeaderKey: "reports.fx_detail.col.historical", Align: AlignRight},
		{HeaderKey: "reports.fx_detail.col.remeasured", Align: AlignRight},
		{HeaderKey: "reports.fx_detail.col.gain_loss", Align: AlignRight},
		{HeaderKey: "reports.fx_detail.col.rate", Align: AlignRight},
		{HeaderKey: "reports.fx_detail.col.rate_date", Align: AlignLeft},
	}
}

// fxHeaderRow builds a section-header row: the sub-name cell, a localized "functional"
// label, and the functional currency code, with the remaining amount columns blank.
func fxHeaderRow(subCell Cell, functional string) []Cell {
	return []Cell{
		subCell,
		LabelCell("reports.fx_detail.functional"),
		TextCell(functional),
		BlankMoneyCell(),
		BlankMoneyCell(),
		BlankMoneyCell(),
		TextCell(""),
		TextCell(""),
	}
}

// fxTotalRow builds a total row: a label in the account column, the money figure in the
// FX gain/(loss) column (col 6), everything else blank -- so a total lands under the
// gain/loss it sums.
func fxTotalRow(label, amount Cell) []Cell {
	return []Cell{
		label,
		TextCell(""),
		BlankMoneyCell(),
		BlankMoneyCell(),
		BlankMoneyCell(),
		amount,
		TextCell(""),
		TextCell(""),
	}
}

// fxNoteRow builds the empty-state note row: the note label in the account column, the
// rest blank.
func fxNoteRow(note Cell) []Cell {
	return []Cell{
		note,
		TextCell(""),
		BlankMoneyCell(),
		BlankMoneyCell(),
		BlankMoneyCell(),
		BlankMoneyCell(),
		TextCell(""),
		TextCell(""),
	}
}

// fxRate formats a closing rate as a stable decimal string for the rate column (there
// is no numeric cell type; a TextCell keeps the golden deterministic). FormatFloat with
// -1 precision emits the shortest exact round-trip form, so the value is byte-stable
// across machines (never %v/%g locale drift).
func fxRate(rate float64) string {
	return strconv.FormatFloat(rate, 'f', -1, 64)
}
