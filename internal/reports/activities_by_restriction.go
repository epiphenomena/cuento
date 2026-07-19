package reports

import (
	"context"
	"sort"

	"cuento/internal/store"
)

// ActivitiesByRestrictionReportID is the id (URL slug + registry key) of the
// activities-by-restriction report (p15.9): the FASB nonprofit STATEMENT OF ACTIVITIES
// with the two donor-restriction columns — Without Donor Restrictions and With Donor
// Restrictions — plus a Total column. It is the net-asset flow statement a 990 preparer
// reads to see how each restriction class changed over the period.
//
// GROUP "financial" (the code-declared Groups() doc files p15.9 here — it is a core
// financial statement, alongside the trial balance / balance sheet / income statement).
//
// NATIVE currency, per-currency rows (Line | Currency | Without | With | Total). The
// report is NOT converted: the DERIVED "net assets released from restrictions" line must
// equal the SUM of p15.8's single-fund applied figures across the restricted funds by
// EXACT int64 equality (the cross-report bridge the step requires), and p15.8 is native;
// converting either side would break the exact cross-check and would need per-fund,
// per-month activity the toolkit does not expose. Multi-currency is shown as one row per
// currency, exactly as the balance-sheet detail mode presents its by-restriction net-
// asset split. (DECISIONS p15.9.)
//
// ROWS (per currency, currencies sorted):
//   - Revenue and support — each split classified by the fund it lands in: a split in a
//     RESTRICTED fund (Restriction != "", i.e. Beca Agua / Building Fund) => With; a NULL
//     / unrestricted split => Without. Shown POSITIVE (revenue is a net-debit credit).
//   - Net assets released from restrictions — DERIVED (D20, no journaled transfer) =
//     Σ restricted funds' APPLICATIONS in the period (expense + non-expense, reusing
//     p15.8's FundPeriodStatement). +released in Without, −released in With, so the row
//     NETS TO ZERO in Total: it MOVES restricted resources to unrestricted as they are
//     spent. The released cells DRILL to the restricted-fund application splits (a fund
//     SET drill, since the figure spans every restricted fund).
//   - Total support and revenue — revenue ± released, per column (a subtotal).
//   - Expenses — all-fund expense activity, in Without only (functional expenses reduce
//     unrestricted net assets AFTER the release). Shown POSITIVE (a net-debit debit).
//   - Change in net assets — (revenue ± released) − expenses, per column. Without + With
//     == Total by construction, and Total == revenue − expenses (released nets out).
const ActivitiesByRestrictionReportID = "activities_by_restriction"

// registerActivitiesByRestriction registers the activities-by-restriction report (p15.9)
// into reg under the "financial" group. It offers only the period (from/to); the shared
// web params form renders the period + the always-present subsidiary scope selector.
func registerActivitiesByRestriction(reg *Registry) {
	reg.Register(Report{
		ID:         ActivitiesByRestrictionReportID,
		TitleKey:   "reports.activities_by_restriction.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{Period: true},
		Run:        runActivitiesByRestriction,
		// p27.4b: NOT ProgramDimensioned. Its whole subject is the WITH/WITHOUT-restriction
		// split, which is a FUND property (Restriction), not a program one. The With column
		// and the "released from restrictions" line derive from restricted funds' period
		// statements (FundPeriodStatement) and fold in program-less non-expense capital
		// applications -- no program dimension. Program-filtering only the revenue/expense
		// totals while the fund-derived restriction figures stayed org-wide would produce an
		// incoherent report whose change-in-net-assets no longer reconciles AND still leaks
		// org-wide rows. So a purely program-scoped grant does NOT reach it; income_statement
		// keeps the "financial" group's program-scoped reach.
	})
}

// abrColumn is one restriction column's per-currency accumulator (minor units).
type abrColumn map[string]int64

// runActivitiesByRestriction computes the statement of activities by restriction over
// [From,To] in the scope: it classifies revenue into With/Without by fund tagging,
// derives the released line from the restricted funds' applications (D20), places
// expenses in Without, and closes with the per-column change in net assets.
func runActivitiesByRestriction(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	scope := Scope{Sub: p.Scope}

	// Account types (revenue / expense / asset), read once — also the drill account sets.
	tree, err := tk.Store().Tree(ctx, "en", nil)
	if err != nil {
		return Table{}, err
	}
	revenueIDs, expenseIDs := revenueAndExpenseIDs(tree)

	// Native per-account activity over the period (revenue net-debit negative; expense
	// positive). One place: the total revenue/expense figures per currency.
	act, err := tk.Activity(ctx, scope, p.From, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	revenueTotal := abrColumn{} // per currency, POSITIVE (−Σ revenue net-debit)
	expenseTotal := abrColumn{} // per currency, POSITIVE (Σ expense net-debit)
	revSet := idSet(revenueIDs)
	expSet := idSet(expenseIDs)
	for acct, amts := range act {
		for _, a := range amts {
			switch {
			case revSet[int64(acct)]:
				revenueTotal[a.Currency] += -a.Minor
			case expSet[int64(acct)]:
				expenseTotal[a.Currency] += a.Minor
			}
		}
	}

	// Restricted funds (Restriction != "") — the With column and the released line come
	// from these funds' period statements (p15.8), so the released figure equals the
	// sum of p15.8's applied figures by construction.
	restricted, err := tk.restrictedFundIDs(ctx)
	if err != nil {
		return Table{}, err
	}

	revWith := abrColumn{}      // restricted-fund revenue receipts, POSITIVE, per currency
	released := abrColumn{}     // restricted-fund applications (expense + non-expense), per ccy
	capital := map[int64]bool{} // capital-asset accounts a restricted fund applied INTO
	for _, fund := range restricted {
		st, err := tk.FundPeriodStatement(ctx, scope, FundID(fund), p.From, p.To)
		if err != nil {
			return Table{}, err
		}
		for _, ccy := range st.Currencies {
			revWith[ccy] += st.Received[ccy]
			released[ccy] += st.AppliedExpense[ccy] + st.AppliedNonExpense[ccy]
		}
		for id := range st.CapitalAccounts {
			capital[int64(id)] = true
		}
	}
	// The released line's application account set = expense accounts + the capital-asset
	// accounts the restricted funds applied INTO (the Building). Cash / spendable assets
	// are EXCLUDED — they are the source/counterpart of a receipt or spend (a cash debit
	// on a grant receipt is NOT an application), so including them would over-count the
	// released drill by the cash movements. This mirrors p15.8's spendable-vs-capital split.
	appIDs := appendSorted(expenseIDs, setToSorted(capital))

	// Without-revenue = total revenue − With-revenue, per currency (the unrestricted /
	// NULL-fund support: a whole minus its restricted part, no separate NULL-fund query).
	revWithout := abrColumn{}
	for ccy, v := range revenueTotal {
		revWithout[ccy] = v - revWith[ccy]
	}
	for ccy, v := range revWith {
		if _, ok := revenueTotal[ccy]; !ok {
			revWithout[ccy] = -v // a restricted currency with no total: surface the negative
		}
	}

	// The currency section order: the sorted union of every currency any figure uses.
	ccys := abrUnionCurrencies(revenueTotal, expenseTotal, revWith, released)

	b := &abrBuilder{p: p, restricted: restricted}
	b.columns()
	for _, ccy := range ccys {
		relRestricted := drillFunds(restricted)
		// Revenue and support (Without | With | Total). Drillable: the With cell drills
		// the restricted funds' revenue splits; the Without cell is unrestricted (NULL
		// fund) — no NULL-fund drill filter exists, so it is left plain (the p15.8 fund-0
		// precedent). The Total cell is a mixed roll-up, not drilled.
		b.row("reports.activities_by_restriction.revenue", ccy, RowData,
			revWithout[ccy], revWith[ccy],
			nil,
			b.revenueWithDrill(ccy, revenueIDs, relRestricted))

		// Net assets released from restrictions: +released Without, −released With, 0 Total.
		// BOTH signed cells drill to the SAME restricted-fund application splits (a fund
		// SET drill over the expense + capital-asset accounts); their magnitude equals the
		// derived figure (drill.go reconciliation invariant).
		relDrill := b.releasedDrill(ccy, appIDs, relRestricted)
		b.releasedRow("reports.activities_by_restriction.released", ccy, released[ccy], relDrill)

		// Total support and revenue = revenue ± released (a subtotal).
		trWithout := revWithout[ccy] + released[ccy]
		trWith := revWith[ccy] - released[ccy]
		b.row("reports.activities_by_restriction.total_support", ccy, RowSubtotal,
			trWithout, trWith, nil, nil)

		// Expenses — in Without only (With 0, Total == Without). Drillable per the expense
		// account set (all funds; expenses reduce unrestricted net assets after release).
		expDrill := &Drill{
			Scope: int64(p.Scope), AccountIDs: expenseIDs, Currency: ccy,
			Mode: DrillPeriod, From: p.From, To: p.To,
		}
		b.row("reports.activities_by_restriction.expenses", ccy, RowData,
			expenseTotal[ccy], 0, expDrill, nil)

		// Change in net assets = (revenue ± released) − expenses, per column.
		chWithout := trWithout - expenseTotal[ccy]
		chWith := trWith
		b.row("reports.activities_by_restriction.change", ccy, RowTotal,
			chWithout, chWith, nil, nil)
	}
	return b.table(), nil
}

// abrBuilder accumulates the statement rows. Columns: Line | Currency | Without | With |
// Total. Every money cell is native (the report is not converted).
type abrBuilder struct {
	p          Params
	restricted []int64
	cols       []Column
	rows       []Row
}

func (b *abrBuilder) columns() {
	b.cols = []Column{
		{HeaderKey: "reports.activities_by_restriction.col.line", Align: AlignLeft},
		{HeaderKey: "reports.activities_by_restriction.col.currency", Align: AlignLeft},
		{HeaderKey: "reports.activities_by_restriction.col.without", Align: AlignRight},
		{HeaderKey: "reports.activities_by_restriction.col.with", Align: AlignRight},
		{HeaderKey: "reports.activities_by_restriction.col.total", Align: AlignRight},
	}
}

// row appends one statement line: label, currency, then the Without / With / Total money
// cells (Total = Without + With). withoutDrill drills the Without cell; withDrill the
// With cell; the Total cell (a mixed roll-up) is never drilled. A nil drill => plain cell.
func (b *abrBuilder) row(labelKey, ccy string, kind RowKind, without, with int64, withoutDrill, withDrill *Drill) {
	total := without + with
	wc := MoneyCell(without, ccy)
	if withoutDrill != nil {
		wc = wc.WithDrill(withoutDrill)
	}
	wic := MoneyCell(with, ccy)
	if withDrill != nil {
		wic = wic.WithDrill(withDrill)
	}
	b.rows = append(b.rows, Row{
		Cells: []Cell{LabelCell(labelKey), TextCell(ccy), wc, wic, MoneyCell(total, ccy)},
		Kind:  kind,
	})
}

// releasedRow appends the "net assets released from restrictions" line: +released in
// Without, −released in With, 0 in Total (the row nets to zero). BOTH signed cells carry
// the SAME fund-SET drill (the restricted-fund application splits); their magnitude equals
// the derived figure. The Total cell is a structural zero (not drilled).
func (b *abrBuilder) releasedRow(labelKey, ccy string, released int64, d *Drill) {
	wc := MoneyCell(released, ccy)
	wic := MoneyCell(-released, ccy)
	if d != nil {
		wc = wc.WithDrill(d)
		wic = wic.WithDrill(d)
	}
	b.rows = append(b.rows, Row{
		Cells: []Cell{LabelCell(labelKey), TextCell(ccy), wc, wic, MoneyCell(0, ccy)},
		Kind:  RowData,
	})
}

// revenueWithDrill builds the With-revenue cell's drill: the restricted funds' revenue
// splits in the period (a fund SET over the revenue account set). Reconciles to the RAW
// net-debit (negative) revenue figure — the drill lists the actual credit splits, whose
// signed sum is the negated positive figure the cell shows (the p15.8 received-drill
// convention).
func (b *abrBuilder) revenueWithDrill(ccy string, revenueIDs []int64, funds []int64) *Drill {
	if len(funds) == 0 || len(revenueIDs) == 0 {
		return nil
	}
	return &Drill{
		Scope: int64(b.p.Scope), AccountIDs: revenueIDs, Currency: ccy, FundIDs: funds,
		Mode: DrillPeriod, From: b.p.From, To: b.p.To,
	}
}

// releasedDrill builds the released cell's fund-SET drill: the restricted funds'
// application splits (expense + capital-asset accounts) over the period. Non-restricted-
// fund splits on the same accounts (unrestricted expenses, the unrestricted Building
// opening) are filtered out by the fund set, so the drilled sum equals the derived
// released figure exactly.
func (b *abrBuilder) releasedDrill(ccy string, appIDs, funds []int64) *Drill {
	if len(funds) == 0 || len(appIDs) == 0 {
		return nil
	}
	return &Drill{
		Scope: int64(b.p.Scope), AccountIDs: appIDs, Currency: ccy, FundIDs: funds,
		Mode: DrillPeriod, From: b.p.From, To: b.p.To,
	}
}

func (b *abrBuilder) table() Table { return Table{Columns: b.cols, Rows: b.rows} }

// --- helpers ----------------------------------------------------------------

// restrictedFundIDs returns the sorted ids of every RESTRICTED fund (Restriction != "":
// purpose / time / perpetual) — the funds whose receipts are With-donor-restriction
// support and whose applications derive the released line (Q3, D20). Fund id 0
// (unrestricted) is never restricted. Mirrors the balance-sheet restrictedNetAssets rule.
func (tk *Toolkit) restrictedFundIDs(ctx context.Context) ([]int64, error) {
	funds, err := tk.store.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, f := range funds {
		if f.Restriction != "" {
			out = append(out, f.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// revenueAndExpenseIDs returns the sorted revenue and expense account id sets from an
// account tree — the total-revenue / total-expense figures' account sets and the drill
// targets (revenue for the With-revenue drill, expense for the released + expense drills).
// (Drill filters by a concrete account set; a placeholder carries no splits, so including
// it is harmless.)
func revenueAndExpenseIDs(tree []store.TreeRow) (revenue, expense []int64) {
	for _, r := range tree {
		switch r.Type {
		case "revenue":
			revenue = append(revenue, r.ID)
		case "expense":
			expense = append(expense, r.ID)
		}
	}
	sort.Slice(revenue, func(i, j int) bool { return revenue[i] < revenue[j] })
	sort.Slice(expense, func(i, j int) bool { return expense[i] < expense[j] })
	return revenue, expense
}

// setToSorted returns the sorted ids of a set.
func setToSorted(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// idSet builds a membership set from an id slice.
func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// drillFunds copies the restricted-fund id slice for a Drill.FundIDs (defensive: the
// drill must not alias the builder's slice, which the row loop reuses).
func drillFunds(ids []int64) []int64 {
	out := make([]int64, len(ids))
	copy(out, ids)
	return out
}

// appendSorted merges two sorted id slices into one sorted, de-duplicated slice (the
// released drill's account set = expense ∪ asset accounts).
func appendSorted(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	// De-dup (the two type sets are disjoint, but keep it robust).
	dedup := out[:0]
	var prev int64 = -1
	for i, v := range out {
		if i == 0 || v != prev {
			dedup = append(dedup, v)
		}
		prev = v
	}
	return dedup
}

// abrUnionCurrencies returns the sorted union of the currencies across the given per-
// currency maps — the report's currency section order.
func abrUnionCurrencies(maps ...abrColumn) []string {
	seen := map[string]bool{}
	for _, m := range maps {
		for ccy := range m {
			seen[ccy] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ccy := range seen {
		out = append(out, ccy)
	}
	sort.Strings(out)
	return out
}
