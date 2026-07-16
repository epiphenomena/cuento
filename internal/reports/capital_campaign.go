package reports

import (
	"context"
	"sort"

	"cuento/internal/store"
)

// CapitalCampaignReportID is the id (URL slug + registry key) of the capital-campaign
// report (p26.51): a QUARTERLY capital-campaign statement scoped to ONE restricted
// fund (the campaign fund, chosen via the FUND param). It is cuento's native analogue
// of the user's Python quarterly capital-campaign statement (campus.py) -- structurally
// the same line items over a quarterly time series, but computed the cuento way
// (per-subsidiary, closing-rate conversion) rather than per-split-date consolidation.
//
// LINE ITEMS (per quarter, plus a cumulative total), reproducing campus.py's structure:
//   - Gross Revenue   -- the fund's revenue inflows in the quarter (per-quarter FLOW).
//   - Gross Expenses  -- the fund's expense applications in the quarter (per-quarter FLOW).
//   - Net Cash        -- Gross Revenue - Gross Expenses (a per-quarter flow).
//   - Capitalized     -- the fund's capital (non-cash) asset position AS OF quarter-end
//     (a cumulative BALANCE): Land + the fixed-asset accounts the campaign deployed into.
//   - Restricted Net Assets (RNA) -- Gross Revenue(cum) - Gross Expenses(cum) - Capitalized
//     (campus.py's exact formula), the unspent/undeployed restricted balance AS OF
//     quarter-end. It equals the fund's SPENDABLE closing balance (the fund opens at 0),
//     the same quantity p15.8's FundStatement.Closing reports.
//
// A trailing CAPITAL DETAIL section lists each capital asset account by NAME (e.g.
// "Land", "Construction in Progress") with its as-of-report-date balance, so a reviewer
// sees the Land line campus.py breaks out -- WITHOUT the report branching on any account
// name or number (rule 11 / p26.46): "Land" appears purely because the chart names an
// account Land, exactly like the balance sheet's account rows.
//
// LAND vs FIXED ASSETS: campus.py splits Land from the fixed-asset rollup by hardcoded
// account NUMBERS. cuento's chart nests Land UNDER a Fixed Assets parent and offers no
// rule-clean way to isolate "Land" or "the fixed-asset subtree" in report code (rule 11
// bars the numbers; p26.46 bars name-substring matching). So the matrix carries ONE
// combined "Capitalized" line (= campus.py Land + Fixed Assets) and the detail section
// shows the per-account breakdown for visibility. (DECISIONS p26.51.)
//
// PLEDGES DUE / UNDEPOSITED (campus.py lines) are OMITTED: cuento fund-tags only a
// subset of the pledge/undeposited splits (a prior review found pledge acct only, and
// ~330/19,082 undeposited splits tagged), so a fund-scoped figure would be materially
// incomplete; the report does not fabricate the untagged portion. (DECISIONS p26.51.)
//
// FX BASIS: figures are CONVERTED to the report's target currency at each quarter-end's
// CLOSING rate (D12, the toolkit's ConvertMinorAt) -- cuento's standard per-period-end
// convention, NOT campus.py's per-split-date rate. This yields a small, EXPECTED residual
// vs the consolidated Python report (per-subsidiary + closing-rate, by design). With no
// target currency the report renders native per-currency rows.
//
// SCOPE: cuento is per-subsidiary; a campaign fund lives in its member subsidiaries (no
// synthetic CONS entity). The report reflects that eliminated/per-sub view. GROUP funds.
const CapitalCampaignReportID = "capital_campaign"

// registerCapitalCampaign registers the capital-campaign report (p26.51) into reg under
// the "funds" group (donor-restricted fund tracking, alongside fund activity). It offers
// the period (from/to), the FUND selector, and the target-currency control; quarterly is
// implied (the report always buckets by quarter).
func registerCapitalCampaign(reg *Registry) {
	reg.Register(Report{
		ID:         CapitalCampaignReportID,
		TitleKey:   "reports.capital_campaign.title",
		Group:      "funds",
		ParamsSpec: ParamsSpec{Period: true, Fund: true, Currency: true},
		Run:        runCapitalCampaign,
	})
}

// campaignQuarter is one quarter's computed cells (native per-currency), before
// conversion: the per-quarter FLOWS and the as-of-quarter-end BALANCES.
type campaignQuarter struct {
	end         string           // quarter-end date (YYYY-MM-DD)
	grossRev    map[string]int64 // per-quarter revenue inflow, per currency (positive)
	grossExp    map[string]int64 // per-quarter expense application, per currency (positive)
	capitalized map[string]int64 // as-of quarter-end capital (Land + fixed) balance, per ccy
	rna         map[string]int64 // as-of quarter-end restricted net assets (spendable), per ccy
}

// runCapitalCampaign builds the quarterly capital-campaign statement for the chosen
// fund. With no fund chosen it returns an empty Table (the framework's nothing-to-show
// rule), so a bare hit still renders 200.
func runCapitalCampaign(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	if p.Fund == 0 {
		return Table{}, nil
	}

	// The fund's whole ledger to the report date (one read; bucketed below). Ordered
	// by (date, split_id) with IsAsset flagged. All the campaign's splits already live
	// only in its subsidiaries, so scope narrows nothing but the store honors it.
	rows, err := tk.store.FundLedger(ctx, p.Fund, p.To)
	if err != nil {
		return Table{}, err
	}

	// Account type per id (revenue / expense / asset) and the resolved display names,
	// read once (names in the request language for the capital-detail section).
	tree, err := tk.store.Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}
	acctType := make(map[int64]string, len(tree))
	acctName := make(map[int64]string, len(tree))
	for _, r := range tree {
		acctType[r.ID] = r.Type
		acctName[r.ID] = r.Name
	}

	// CAPITAL (non-cash) asset accounts: asset accounts that took a DEBIT on a
	// disbursement transaction (a txn with no revenue split for this fund) -- the
	// campaign CAPITALIZING cash into a held asset (Land, Construction). Computed over
	// the FULL span so it is not period-clipped. Mirrors FundStatement's classification.
	capitalAccts := capitalAssetAccounts(rows, acctType)

	// Bucket the span into quarters and compute each quarter's cells. The quarter list
	// runs from From (the campaign start / first activity) to To.
	from := p.From
	if from == "" {
		from = earliestDate(rows)
	}
	if from == "" || from > p.To {
		return Table{}, nil // no activity to show
	}

	var quarters []campaignQuarter
	err = tk.ByPeriod(from, p.To, GranQuarter, func(qFrom, qTo string) error {
		q := campaignQuarter{
			end:         qTo,
			grossRev:    map[string]int64{},
			grossExp:    map[string]int64{},
			capitalized: map[string]int64{},
			rna:         map[string]int64{},
		}
		for _, r := range rows {
			ccy := r.Currency
			inQuarter := r.Date >= qFrom && r.Date <= qTo
			asOf := r.Date <= qTo
			switch acctType[r.AccountID] {
			case "revenue":
				if inQuarter {
					q.grossRev[ccy] += -r.Amount // credit -> positive inflow
				}
			case "expense":
				if inQuarter {
					q.grossExp[ccy] += r.Amount
				}
			case "asset":
				if !asOf {
					continue
				}
				if capitalAccts[r.AccountID] {
					q.capitalized[ccy] += r.Amount
				} else {
					q.rna[ccy] += r.Amount // spendable (cash) asset running balance = RNA
				}
			case "liability":
				// A liability draw is a receipt; a paydown is a non-expense application.
				// The campaign fixture has none, but keep the classification honest so a
				// real campaign with a construction loan reads correctly.
				if !asOf {
					continue
				}
				q.capitalized[ccy] -= r.Amount // net the liability against capital position
			}
		}
		quarters = append(quarters, q)
		return nil
	})
	if err != nil {
		return Table{}, err
	}

	// As-of-report-date capital position per account (the detail section), native.
	capByAccount := map[int64]map[string]int64{}
	for _, r := range rows {
		if !capitalAccts[r.AccountID] {
			continue
		}
		if r.Date > p.To {
			continue
		}
		if capByAccount[r.AccountID] == nil {
			capByAccount[r.AccountID] = map[string]int64{}
		}
		capByAccount[r.AccountID][r.Currency] += r.Amount
	}

	b := &campaignBuilder{tk: tk, ctx: ctx, p: p, target: p.TargetCurrency}

	// --- per-quarter rows: flows (rev/exp/net) + as-of balances (capitalized/RNA).
	cumRev := map[string]int64{}
	cumExp := map[string]int64{}
	for _, q := range quarters {
		for ccy, v := range q.grossRev {
			cumRev[ccy] += v
		}
		for ccy, v := range q.grossExp {
			cumExp[ccy] += v
		}
		if err := b.quarterRow(q); err != nil {
			return Table{}, err
		}
	}

	// --- cumulative total row: cumulative flows + the final as-of balances.
	var final campaignQuarter
	if len(quarters) > 0 {
		final = quarters[len(quarters)-1]
	}
	if err := b.totalRow(cumRev, cumExp, final.capitalized, final.rna); err != nil {
		return Table{}, err
	}

	// --- capital-detail section: one row per capital account, as-of To.
	if err := b.capitalDetail(capByAccount, acctName); err != nil {
		return Table{}, err
	}

	return b.table(), nil
}

// capitalAssetAccounts returns the set of asset accounts the fund CAPITALIZED into: an
// asset account that received a DEBIT (amount > 0) on a DISBURSEMENT transaction (a txn
// with no revenue split for this fund). Every other asset account is spendable (cash).
// Computed over the whole ledger so a quarter's view is not clipped. Mirrors the
// FundStatement capital classification (compute.go) so the two reports agree.
func capitalAssetAccounts(rows []storeFundLedgerRow, acctType map[int64]string) map[int64]bool {
	revenueTxn := map[int64]bool{}
	for _, r := range rows {
		if acctType[r.AccountID] == "revenue" {
			revenueTxn[r.TxnID] = true
		}
	}
	capital := map[int64]bool{}
	for _, r := range rows {
		if acctType[r.AccountID] == "asset" && r.Amount > 0 && !revenueTxn[r.TxnID] {
			capital[r.AccountID] = true
		}
	}
	return capital
}

// earliestDate returns the earliest transaction date across the fund ledger rows (the
// campaign's first activity), or "" when there are none. Rows are date-ordered, so the
// first row's date is the earliest.
func earliestDate(rows []storeFundLedgerRow) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Date
}

// campaignBuilder accumulates the report rows. The report converts every figure to the
// target currency at the relevant quarter-end (or report date) closing rate; with no
// target it renders native single-currency figures (summing across currencies would be
// meaningless, so a native run is only sensible for a single-currency campaign).
type campaignBuilder struct {
	tk     *Toolkit
	ctx    context.Context
	p      Params
	target string
	rows   []Row
}

// columnSet returns the report's fixed column shape: the quarter (or capital-detail
// account name), then the five campaign line-item money columns.
func (b *campaignBuilder) columnSet() []Column {
	return []Column{
		{HeaderKey: "reports.capital_campaign.col.period", Align: AlignLeft},
		{HeaderKey: "reports.capital_campaign.col.gross_revenue", Align: AlignRight},
		{HeaderKey: "reports.capital_campaign.col.gross_expenses", Align: AlignRight},
		{HeaderKey: "reports.capital_campaign.col.net_cash", Align: AlignRight},
		{HeaderKey: "reports.capital_campaign.col.capitalized", Align: AlignRight},
		{HeaderKey: "reports.capital_campaign.col.rna", Align: AlignRight},
	}
}

// convertAt converts a native per-currency map to the target's minor total at the
// closing rate on-or-before d (D12). With no target it requires a single currency and
// returns it raw (a native run only makes sense for a single-currency campaign; a
// multi-currency native run surfaces the first currency's figure, deterministically).
func (b *campaignBuilder) convertAt(byCcy map[string]int64, d string) (int64, string, error) {
	if b.target == "" {
		// Native: sum only makes sense single-currency. Return the sole currency's
		// figure (sorted, deterministic) so a native single-currency campaign reads
		// correctly; a multi-currency native run is a misuse the params default guards.
		for _, ccy := range sortedKeys(byCcy) {
			return byCcy[ccy], ccy, nil
		}
		return 0, "", nil
	}
	var total int64
	for _, ccy := range sortedKeys(byCcy) {
		conv, err := b.tk.ConvertMinorAt(b.ctx, byCcy[ccy], ccy, b.target, d)
		if err != nil {
			return 0, "", err
		}
		total += conv
	}
	return total, b.target, nil
}

// quarterRow appends one quarter's row: the quarter-end date, the per-quarter flows
// (rev/exp/net), and the as-of-quarter-end balances (capitalized/RNA), all converted at
// the quarter-end closing rate.
func (b *campaignBuilder) quarterRow(q campaignQuarter) error {
	rev, ccy, err := b.convertAt(q.grossRev, q.end)
	if err != nil {
		return err
	}
	exp, _, err := b.convertAt(q.grossExp, q.end)
	if err != nil {
		return err
	}
	capital, capCcy, err := b.convertAt(q.capitalized, q.end)
	if err != nil {
		return err
	}
	rna, rnaCcy, err := b.convertAt(q.rna, q.end)
	if err != nil {
		return err
	}
	ccy = firstNonEmpty(ccy, capCcy, rnaCcy)
	net := rev - exp
	b.rows = append(b.rows, Row{
		Cells: []Cell{
			DateCell(q.end),
			MoneyCell(rev, ccy),
			MoneyCell(exp, ccy),
			MoneyCell(net, ccy),
			MoneyCell(capital, ccy),
			MoneyCell(rna, ccy),
		},
		Kind: RowData,
	})
	return nil
}

// totalRow appends the cumulative-total row: cumulative flows + the final as-of
// balances, converted at the report date.
func (b *campaignBuilder) totalRow(cumRev, cumExp, cap, rna map[string]int64) error {
	rev, ccy, err := b.convertAt(cumRev, b.p.To)
	if err != nil {
		return err
	}
	exp, _, err := b.convertAt(cumExp, b.p.To)
	if err != nil {
		return err
	}
	c, capCcy, err := b.convertAt(cap, b.p.To)
	if err != nil {
		return err
	}
	r, rnaCcy, err := b.convertAt(rna, b.p.To)
	if err != nil {
		return err
	}
	ccy = firstNonEmpty(ccy, capCcy, rnaCcy)
	b.rows = append(b.rows, Row{
		Cells: []Cell{
			LabelCell("reports.capital_campaign.total"),
			MoneyCell(rev, ccy),
			MoneyCell(exp, ccy),
			MoneyCell(rev-exp, ccy),
			MoneyCell(c, ccy),
			MoneyCell(r, ccy),
		},
		Kind: RowTotal,
	})
	return nil
}

// capitalDetail appends the per-account capital breakdown as-of the report date: a
// section header, then one row per capital account keyed by its NAME (a proper noun),
// its as-of balance in the Capitalized column (other columns blank). Accounts are
// ordered by id for determinism.
func (b *campaignBuilder) capitalDetail(byAccount map[int64]map[string]int64, name map[int64]string) error {
	if len(byAccount) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(byAccount))
	for id := range byAccount {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	b.rows = append(b.rows, Row{
		Cells: []Cell{
			LabelCell("reports.capital_campaign.capital_detail"),
			BlankMoneyCell(), BlankMoneyCell(), BlankMoneyCell(), BlankMoneyCell(), BlankMoneyCell(),
		},
		Kind: RowSubtotal,
	})
	for _, id := range ids {
		amt, ccy, err := b.convertAt(byAccount[id], b.p.To)
		if err != nil {
			return err
		}
		b.rows = append(b.rows, Row{
			Cells: []Cell{
				TextCell(name[id]),
				BlankMoneyCell(), BlankMoneyCell(), BlankMoneyCell(),
				MoneyCell(amt, ccy),
				BlankMoneyCell(),
			},
			Indent: 1,
			Kind:   RowData,
		})
	}
	return nil
}

func (b *campaignBuilder) table() Table {
	return Table{Columns: b.columnSet(), Rows: b.rows}
}

// firstNonEmpty returns the first non-empty currency code among the args (a native run
// may have a figure in only some columns; the row's currency is the first that exists).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// storeFundLedgerRow is a local alias for the store's FundLedgerRow so the campaign
// helpers read without spelling the store type at every use.
type storeFundLedgerRow = store.FundLedgerRow
