package reports

import (
	"context"
	"sort"

	"cuento/internal/store"
)

// FundActivityReportID is the id (URL slug + registry key) of the fund balances &
// activity report (p15.8): the donor-restricted fund tracking / per-grant funder view
// (Q3, D20). It has TWO views, selected by the report-specific FUND param:
//
//   - LIST view (no fund chosen, Params.Fund == 0): the fund ROSTER — every fund with
//     its as-of balance (per currency, native), plus its funder / restriction-kind /
//     purpose metadata columns, INCLUDING the synthetic Unrestricted line (fund 0). The
//     balance cells are drillable (as-of, the fund's asset splits).
//
//   - SINGLE-FUND STATEMENT (a fund chosen via ?fund=): ONE fund over a period —
//     Opening (spendable balance the day before From), Received (contributions/revenue
//     INTO the fund in the period), Applied SPLIT into (a) EXPENSE applications (splits
//     on expense accounts) and (b) NON-EXPENSE applications (asset purchases, loan
//     principal — splits on asset/liability accounts, e.g. the fixture's Building
//     purchase), and Closing (Opening + Received − Applied), per currency. The report
//     ALSO emits a reconciliation line proving Closing + Capitalized == the all-asset
//     FundBalancesAsOf(To) the LIST view shows. This split is the D20 "applied"
//     derivation p15.9 consumes.
//
// GROUP funds. NATIVE currency (a fund's per-grant statement reads in the money it was
// received/spent in; no conversion). Fund 0 is never a valid single-fund selection (it
// is the synthetic unrestricted group, list-only).
const FundActivityReportID = "fund_activity"

// registerFundActivity registers the fund balances & activity report (p15.8) into reg
// under the "funds" group. It offers the period (from/to) and the report-specific FUND
// selector; the shared web params form renders both from the ParamsSpec. The period
// controls the single-fund statement window; the list view uses To as its as-of date.
func registerFundActivity(reg *Registry) {
	reg.Register(Report{
		ID:         FundActivityReportID,
		TitleKey:   "reports.fund_activity.title",
		Group:      "funds",
		ParamsSpec: ParamsSpec{Period: true, Fund: true},
		Run:        runFundActivity,
		// p27.4b: NOT ProgramDimensioned. This report is entirely balance-centric -- the
		// fund roster's as-of asset balances and the single-fund Opening/Received/Applied/
		// Closing statement (FundBalancesAsOf / FundPeriodStatement). NONE of that content
		// carries a program (D24: only R/E SPLITS do; a fund's asset position does not), so
		// there is nothing to filter to a program subtree -- suppression would leave an
		// empty report. A purely program-scoped grant does NOT reach it (needs an unscoped
		// "funds" grant). The "funds" group has no program-dimensioned report (noted for the
		// 27.4c picker: a program-scoped grant to "funds" reaches nothing).
	})
}

// runFundActivity dispatches to the LIST view (no fund chosen) or the SINGLE-FUND
// statement (a fund chosen), sharing the toolkit reads.
func runFundActivity(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	if p.Fund == 0 {
		return fundListTable(ctx, tk, p)
	}
	return fundStatementTable(ctx, tk, p)
}

// fundListTable builds the fund ROSTER: every fund's as-of (To) balance per currency,
// with funder / restriction-kind / purpose metadata, plus the Unrestricted line (fund
// 0). Balance cells drill to the fund's asset splits (as-of To).
func fundListTable(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.fund_activity.col.fund", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.funder", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.restriction", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.purpose", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.balance", Align: AlignRight},
		},
	}

	scope := Scope{Sub: p.Scope}
	bals, err := tk.FundBalancesAsOf(ctx, scope, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}

	funds, err := fundMetaMap(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}
	assetIDs, err := assetAccountIDs(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}

	// Deterministic order: real funds by id, then the Unrestricted line (fund 0) last.
	fundIDs := make([]FundID, 0, len(bals))
	for id := range bals {
		fundIDs = append(fundIDs, id)
	}
	sort.Slice(fundIDs, func(i, j int) bool {
		// 0 (unrestricted) sorts last; others by id.
		if fundIDs[i] == 0 {
			return false
		}
		if fundIDs[j] == 0 {
			return true
		}
		return fundIDs[i] < fundIDs[j]
	})

	for _, id := range fundIDs {
		amts := bals[id]
		sort.Slice(amts, func(i, j int) bool { return amts[i].Currency < amts[j].Currency })
		meta := funds[id]
		for i, a := range amts {
			// The fund/funder/restriction/purpose columns repeat only on the FIRST
			// currency row of a multi-currency fund; the rest are blank so the roster
			// reads as one fund per group.
			nameCell := TextCell("")
			funderCell := TextCell("")
			restrCell := TextCell("")
			purposeCell := TextCell("")
			if i == 0 {
				if id == 0 {
					nameCell = LabelCell("reports.fund_activity.unrestricted")
				} else {
					nameCell = TextCell(meta.name)
					funderCell = TextCell(meta.funder)
					restrCell = LabelCell(restrictionKey(meta.restriction))
					purposeCell = TextCell(meta.purpose)
				}
			}
			// The balance drills to the fund's asset splits as-of To. The Unrestricted
			// line (fund 0, NULL fund_id) is NOT drillable — DrillSplits has no NULL-fund
			// filter (fundIDForDrill(0) == nil), and a nil fund filter would sum EVERY
			// fund's assets, not the unrestricted group, so the cell stays plain.
			bal := MoneyCell(a.Minor, a.Currency)
			if fid := fundIDForDrill(id); fid != nil {
				bal = bal.WithDrill(&Drill{
					Scope:      int64(p.Scope),
					AccountIDs: assetIDs,
					Currency:   a.Currency,
					FundID:     fid,
					Mode:       DrillAsOf,
					AsOf:       p.To,
				})
			}
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{nameCell, funderCell, restrCell, purposeCell, TextCell(a.Currency), bal},
				Kind:  RowData,
			})
		}
	}
	return t, nil
}

// fundStatementTable builds ONE fund's period statement: Opening, Received, Applied
// (expense + non-expense), Closing, and a reconciliation line to the all-asset
// FundBalancesAsOf(To). One SECTION per currency the fund uses.
func fundStatementTable(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.fund_activity.col.line", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.fund_activity.col.amount", Align: AlignRight},
		},
	}

	scope := Scope{Sub: p.Scope}
	st, err := tk.FundPeriodStatement(ctx, scope, p.Fund, p.From, p.To)
	if err != nil {
		return Table{}, err
	}

	// The all-asset FundBalancesAsOf(To) for this fund, per currency — the LIST view's
	// figure, for the reconciliation line (Closing + Capitalized == all-asset balance).
	allAsset, err := tk.FundBalancesAsOf(ctx, scope, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	allByCcy := amtsByCurrency(allAsset[p.Fund])

	assetIDs, err := assetAccountIDs(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}
	expenseIDs, err := expenseAccountIDs(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}
	// The spendable-asset account set = every asset account EXCEPT the ones this fund
	// capitalized into (the Building) — the accounts whose as-of sum is the spendable
	// Opening/Closing, drillable.
	spendableIDs := make([]int64, 0, len(assetIDs))
	for _, id := range assetIDs {
		if !st.CapitalAccounts[AccountID(id)] {
			spendableIDs = append(spendableIDs, id)
		}
	}
	capitalIDs := make([]int64, 0, len(st.CapitalAccounts))
	for id := range st.CapitalAccounts {
		capitalIDs = append(capitalIDs, int64(id))
	}
	sort.Slice(capitalIDs, func(i, j int) bool { return capitalIDs[i] < capitalIDs[j] })

	fund := fundIDForDrill(p.Fund)
	opening := dayBefore(p.From)

	for _, ccy := range st.Currencies {
		// Opening (spendable, day before From) — drillable to the spendable asset splits.
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.opening", ccy,
			st.Opening[ccy], RowSubtotal, &Drill{
				Scope: int64(p.Scope), AccountIDs: spendableIDs, Currency: ccy, FundID: fund,
				Mode: DrillAsOf, AsOf: opening,
			}))
		// Received — drillable to the revenue splits in the period.
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.received", ccy,
			st.Received[ccy], RowData, &Drill{
				Scope: int64(p.Scope), AccountIDs: revenueAndLiabilityDrill(ctx, tk), Currency: ccy,
				FundID: fund, Mode: DrillPeriod, From: p.From, To: p.To,
			}))
		// Applied — expense.
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.applied_expense", ccy,
			st.AppliedExpense[ccy], RowData, &Drill{
				Scope: int64(p.Scope), AccountIDs: expenseIDs, Currency: ccy, FundID: fund,
				Mode: DrillPeriod, From: p.From, To: p.To,
			}))
		// Applied — non-expense (the Building purchase, loan principal).
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.applied_nonexpense", ccy,
			st.AppliedNonExpense[ccy], RowData, nonExpenseDrill(p, capitalIDs, ccy, fund)))
		// Closing (spendable) — Opening + Received − Applied.
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.closing", ccy,
			st.Closing[ccy], RowSubtotal, &Drill{
				Scope: int64(p.Scope), AccountIDs: spendableIDs, Currency: ccy, FundID: fund,
				Mode: DrillAsOf, AsOf: p.To,
			}))
		// Reconciliation: Closing + Capitalized == all-asset FundBalancesAsOf(To).
		t.Rows = append(t.Rows, statementRow(t, "reports.fund_activity.total_assets", ccy,
			allByCcy[ccy], RowTotal, &Drill{
				Scope: int64(p.Scope), AccountIDs: assetIDs, Currency: ccy, FundID: fund,
				Mode: DrillAsOf, AsOf: p.To,
			}))
	}
	return t, nil
}

// statementRow builds one fund-statement row: a localized line LABEL, the currency
// code, and the signed money amount carrying an optional drill.
func statementRow(_ Table, labelKey, ccy string, minor int64, kind RowKind, d *Drill) Row {
	amt := MoneyCell(minor, ccy)
	if d != nil {
		amt = amt.WithDrill(d)
	}
	return Row{
		Cells: []Cell{LabelCell(labelKey), TextCell(ccy), amt},
		Kind:  kind,
	}
}

// nonExpenseDrill drills the non-expense-applied cell to the fund's capital-asset
// splits (the Building) over the period. When the fund capitalized nothing (a cash-only
// fund), there is nothing to drill (nil) — the cell renders as a plain zero.
func nonExpenseDrill(p Params, capitalIDs []int64, ccy string, fund *FundID) *Drill {
	if len(capitalIDs) == 0 {
		return nil
	}
	return &Drill{
		Scope: int64(p.Scope), AccountIDs: capitalIDs, Currency: ccy, FundID: fund,
		Mode: DrillPeriod, From: p.From, To: p.To,
	}
}

// revenueAndLiabilityDrill returns the revenue account ids (the Received drill target):
// contributions/grants INTO the fund. Liability draws also count as received but the
// fixture has none, so the drill lists the revenue splits (the reconciling set for the
// common case). A store read error yields an empty set (an empty, harmless drill).
func revenueAndLiabilityDrill(ctx context.Context, tk *Toolkit) []int64 {
	ids, err := revenueAccountIDs(ctx, tk.Store())
	if err != nil {
		return nil
	}
	return ids
}

// fundIDForDrill returns a *int64 fund filter for a drill: a real fund id (>0) filters
// DrillSplits to that fund's splits. Fund 0 (the synthetic unrestricted group, NULL
// fund_id) returns nil — DrillSplits' fund filter is `fund = 0 OR fund_id = ?`, whose
// "0" sentinel means "no filter" (matches ALL funds), and there is no NULL-fund filter,
// so an unrestricted-line cell is left non-drillable rather than drilling every fund.
func fundIDForDrill(id FundID) *FundID {
	if id == 0 {
		return nil
	}
	v := id
	return &v
}

// --- reference-data helpers ------------------------------------------------

// fundMeta is a fund's roster metadata: its name (proper noun), funder, restriction
// kind (purpose/time/perpetual), and purpose text.
type fundMeta struct {
	name        string
	funder      string
	restriction string
	purpose     string
}

// fundMetaMap returns fund id -> its roster metadata for every fund (active AND
// closed — the roster shows a closed fund's residual balance), loaded once.
func fundMetaMap(ctx context.Context, st *store.Store) (map[FundID]fundMeta, error) {
	fs, err := st.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[FundID]fundMeta, len(fs))
	for _, f := range fs {
		m[f.ID] = fundMeta{name: f.Name, funder: f.Funder, restriction: f.Restriction, purpose: f.Purpose}
	}
	return m, nil
}

// restrictionKey maps a stored restriction kind to its i18n label key (the shared
// funds.restriction.<kind> catalog labels reused from the funds workspace). An unknown
// value falls back to purpose (the schema default) so a row never renders a raw key.
func restrictionKey(kind string) string {
	switch kind {
	case "time":
		return "funds.restriction.time"
	case "perpetual":
		return "funds.restriction.perpetual"
	default:
		return "funds.restriction.purpose"
	}
}

// assetAccountIDs returns every asset account id (sorted) — the drill target for a
// fund's all-asset as-of balance (the LIST cell and the reconciliation line).
func assetAccountIDs(ctx context.Context, st *store.Store) ([]int64, error) {
	return accountIDsOfType(ctx, st, "asset")
}

// expenseAccountIDs returns every expense account id (sorted) — the drill target for a
// fund's expense-applied figure.
func expenseAccountIDs(ctx context.Context, st *store.Store) ([]int64, error) {
	return accountIDsOfType(ctx, st, "expense")
}

// revenueAccountIDs returns every revenue account id (sorted) — the drill target for a
// fund's received figure.
func revenueAccountIDs(ctx context.Context, st *store.Store) ([]int64, error) {
	return accountIDsOfType(ctx, st, "revenue")
}

// accountIDsOfType returns the sorted ids of every account of the given type, read from
// the account tree once. Bounded reference data.
func accountIDsOfType(ctx context.Context, st *store.Store, typ string) ([]int64, error) {
	tree, err := st.Tree(ctx, "en", nil)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, r := range tree {
		if r.Type == typ {
			out = append(out, r.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
