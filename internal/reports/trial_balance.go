package reports

import (
	"context"
	"sort"
)

// TrialBalanceReportID is the id (URL slug + registry key) of the trial-balance
// report (p15.3): the first real financial statement and the exemplar the rest of
// Phase 15 copies. A trial balance lists every account's as-of balance in the
// scope's descendant closure (D18), one row per (account, currency) so a
// multi-currency account (FX Clearing holds USD and MXN) is shown honestly, with a
// NATIVE amount column and a CONVERTED column (target currency at the as-of closing
// rate, D12). Balances are net-debit signed int64 minor units (D2): assets/expenses
// positive, revenue/liability/equity negative. The whole point of a trial balance is
// that it BALANCES — the per-currency native signed sum is exactly zero — so the
// report emits a total row per currency (native) plus a grand converted total, and
// the golden test asserts those totals are zero.
const TrialBalanceReportID = "trial_balance"

// registerTrialBalance registers the trial-balance report (p15.3) into reg under the
// "financial" group. It is the first report registered in Default(), replacing the
// p15.1 framework smoke report.
func registerTrialBalance(reg *Registry) {
	reg.Register(Report{
		ID:         TrialBalanceReportID,
		TitleKey:   "reports.trial_balance.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{AsOf: true, Currency: true},
		Run:        runTrialBalance,
	})
}

// runTrialBalance computes the trial-balance Table. It reads the scope's per-account
// per-currency as-of balances through the toolkit (BalancesAsOf, RateNone), walks the
// account tree in pre-order for STABLE row order and resolved account NAMES (in the
// request language, p.LangOr), and emits one data row per (account, currency):
// account name, currency code, native signed amount, and the amount converted to the
// target currency at the as-of closing rate (ConvertMinorAt per cell — the exact D12
// rule the toolkit uses, applied per currency so a multi-currency account's cells stay
// aligned instead of being collapsed to one sum). It closes with a total row per
// currency (native signed sum — zero on a balanced ledger) and a grand converted
// total (the sum of every converted cell). Every row's amount columns carry the same
// currency the renderers format from, so both HTML (per-user) and CSV (machine-plain,
// which drops the currency code — hence the explicit currency column) are unambiguous.
func runTrialBalance(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	balances, err := tk.BalancesAsOf(ctx, Scope{Sub: p.Scope}, p.AsOf, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}

	// Tree gives pre-order (stable) account order and resolved names in the request
	// language. Only accounts that carry a balance in this scope emit rows.
	tree, err := tk.Store().Tree(ctx, p.LangOr(), nil)
	if err != nil {
		return Table{}, err
	}

	// target is the converted-column currency. It is normally resolved by the web
	// layer from the scope base (ParamsSpec.Currency); an empty target (a hand-built
	// Params with no currency, e.g. the scope test) makes the converted column mirror
	// the native amount — handled per-cell below.
	target := p.TargetCurrency

	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.trial_balance.col.account", Align: AlignLeft},
			{HeaderKey: "reports.trial_balance.col.currency", Align: AlignLeft},
			{HeaderKey: "reports.trial_balance.col.native", Align: AlignRight},
			{HeaderKey: "reports.trial_balance.col.converted", Align: AlignRight},
		},
	}

	// Native per-currency running totals (must net to zero — the balancing check) and
	// the converted grand total (sum of every converted cell).
	nativeTotal := make(map[string]int64)
	var nativeOrder []string
	seenCcy := make(map[string]bool)
	var convertedTotal int64

	for _, node := range tree {
		amts, ok := balances[node.ID]
		if !ok {
			continue
		}
		// Deterministic per-account currency order (BalancesAsOf returns a slice built
		// from a map iteration upstream, so sort here rather than trust its order).
		cur := make([]CurAmt, len(amts))
		copy(cur, amts)
		sort.Slice(cur, func(i, j int) bool { return cur[i].Currency < cur[j].Currency })

		for _, a := range cur {
			conv := a.Minor
			if target != "" {
				conv, err = tk.ConvertMinorAt(ctx, a.Minor, a.Currency, target, p.AsOf)
				if err != nil {
					return Table{}, err
				}
			}
			convCcy := target
			if convCcy == "" {
				convCcy = a.Currency
			}
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					TextCell(node.Name),
					TextCell(a.Currency),
					MoneyCell(a.Minor, a.Currency),
					MoneyCell(conv, convCcy),
				},
				Kind: RowData,
			})
			if !seenCcy[a.Currency] {
				seenCcy[a.Currency] = true
				nativeOrder = append(nativeOrder, a.Currency)
			}
			nativeTotal[a.Currency] += a.Minor
			convertedTotal += conv
		}
	}
	sort.Strings(nativeOrder)

	// One native total row per currency (each is exactly zero on a balanced ledger —
	// that IS the trial balance). The native column carries the per-currency sum; the
	// converted column is blank (the converted grand total is the following row).
	for _, ccy := range nativeOrder {
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				LabelCell("reports.trial_balance.total_native"),
				TextCell(ccy),
				MoneyCell(nativeTotal[ccy], ccy),
				BlankMoneyCell(),
			},
			Kind: RowSubtotal,
		})
	}

	// The converted grand total (target currency): the sum of every converted cell.
	// On the fixture the per-account rounding residuals cancel and this is exactly
	// zero, but that is a data property, not a guarantee — a report is a trial balance
	// because its NATIVE per-currency sums are zero; the converted total is presented
	// so a reviewer sees the whole statement reconciled in one currency.
	convCcy := target
	t.Rows = append(t.Rows, Row{
		Cells: []Cell{
			LabelCell("reports.trial_balance.total_converted"),
			TextCell(""),
			BlankMoneyCell(),
			MoneyCell(convertedTotal, convCcy),
		},
		Kind: RowTotal,
	})

	return t, nil
}
