package reports

import (
	"context"
	"sort"
	"time"

	"cuento/internal/store"
)

// AccountLedgerReportID is the id (URL slug + registry key) of the account-ledger
// report (p15.6): a printable REGISTER for ONE account over a period, with an
// OPENING and CLOSING balance framing the in-range lines. Where the trial balance
// lists every account's as-of balance in one line, the account ledger drills into a
// single account and prints its movement: opening balance (the account's balance the
// day BEFORE `from`), then every split in [from,to] in date order — its date, the
// payee/description, its FUND (the split's fund, or "Unrestricted" for a nil fund),
// its signed net-debit amount (D2), and a RUNNING balance — closing with the
// account's as-of-`to` balance. By construction opening + Σ(range lines) == closing
// (cumulative(to) = cumulative(from-1) + activity(from..to)); the golden asserts it
// per currency against two INDEPENDENT queries (BalancesAsOf and DrillSplits).
//
// PARAMETERS: an ACCOUNT (report-specific, ParamsSpec.Account) and a PERIOD
// (from/to). No target-currency conversion — the ledger prints NATIVE amounts, one
// SECTION per currency the account holds (FX Clearing holds USD and MXN), each with
// its own opening/lines/closing and an INDEPENDENT running balance, so currencies are
// never mixed in one running total. The scope (D18, always present) narrows the lines
// to the scope's descendant closure — an account posted across subsidiaries (the
// intercompany/FX accounts) shows only the in-scope splits, and opening/closing use
// the same scope, so the identity still holds.
//
// LINE -> TXN (p12.4): each line's date cell carries WithTxn(txnID), which the web
// layer renders as a link to the transaction editor/history — the reviewer clicks a
// line to open its entry. OPENING/CLOSING drill (p15.3d): the opening and closing
// balance cells carry an as-of Drill (the transactions producing that cumulative
// balance), so a reviewer can inspect the pre-range history behind the opening figure.
//
// NO ACCOUNT CHOSEN (Account == 0): the report returns an empty Table (the framework's
// nothing-to-show rule), so a bare /reports/account_ledger hit renders 200 with just
// the params form (the permission-matrix / scope-selector test path).
const AccountLedgerReportID = "account_ledger"

// registerAccountLedger registers the account-ledger report (p15.6) into reg under the
// "financial" group. It offers the period (from/to) and the report-specific ACCOUNT
// selector; the shared web params form renders both from the ParamsSpec.
func registerAccountLedger(reg *Registry) {
	reg.Register(Report{
		ID:         AccountLedgerReportID,
		TitleKey:   "reports.account_ledger.title",
		Group:      "financial",
		ParamsSpec: ParamsSpec{Period: true, Account: true},
		Run:        runAccountLedger,
	})
}

// runAccountLedger computes the account-ledger Table. It reads the chosen account's
// per-currency opening balance (as of the day before From), the in-range splits
// (DrillSplits over [From,To], per currency), and the closing balance (as of To) —
// all in the scope's descendant closure (D18) — and renders one section per currency:
// an opening row, one data row per split (date linked to its txn, payee/description,
// fund, amount, running balance), and a closing row. The running balance starts at
// opening and accumulates each line's amount; the closing row equals opening + Σlines,
// which also equals the independently-queried as-of-To balance (asserted in the test).
func runAccountLedger(ctx context.Context, tk *Toolkit, p Params) (Table, error) {
	t := Table{
		Columns: []Column{
			{HeaderKey: "reports.account_ledger.col.date", Align: AlignLeft},
			{HeaderKey: "reports.account_ledger.col.description", Align: AlignLeft},
			{HeaderKey: "reports.account_ledger.col.fund", Align: AlignLeft},
			{HeaderKey: "reports.account_ledger.col.amount", Align: AlignRight},
			{HeaderKey: "reports.account_ledger.col.balance", Align: AlignRight},
		},
	}

	// No account chosen: an empty table (the framework's nothing-to-show rule), so a
	// bare hit renders 200 with just the params form.
	if p.Account == 0 {
		return t, nil
	}

	scope := Scope{Sub: p.Scope}
	opening := dayBefore(p.From)

	// Opening (as of the day before From) and closing (as of To) balances for the
	// account, per currency, native — two INDEPENDENT cumulative queries. Filter to the
	// chosen account (BalancesAsOf returns the whole scope).
	openBal, err := tk.BalancesAsOf(ctx, scope, opening, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	closeBal, err := tk.BalancesAsOf(ctx, scope, p.To, ConvertOpts{Mode: RateNone})
	if err != nil {
		return Table{}, err
	}
	openByCcy := amtsByCurrency(openBal[p.Account])
	closeByCcy := amtsByCurrency(closeBal[p.Account])

	// The currency set: every currency the account holds at opening OR close (an account
	// whose whole balance moved into a currency mid-range still needs that section).
	ccys := unionCurrencies(openByCcy, closeByCcy)

	// Name maps (payee, fund) resolved once (bounded reference data).
	payees, err := payeeNames(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}
	funds, err := fundNames(ctx, tk.Store())
	if err != nil {
		return Table{}, err
	}

	for _, ccy := range ccys {
		// Opening row: the cumulative balance the day before From, drillable to the
		// transactions that produced it (as-of, this account+currency, this scope).
		open := openByCcy[ccy]
		openDrill := &Drill{
			Scope:      p.Scope,
			AccountIDs: []int64{p.Account},
			Currency:   ccy,
			Mode:       DrillAsOf,
			AsOf:       opening,
		}
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				DateCell(opening),
				LabelCell("reports.account_ledger.opening"),
				BlankMoneyCell(),
				BlankMoneyCell(),
				MoneyCell(open, ccy).WithDrill(openDrill),
			},
			Kind: RowSubtotal,
		})

		// In-range lines: every split of the account in [From,To], this currency, this
		// scope (descendant closure), ordered (date, split_id) — the SAME filter the
		// opening/closing balances close over, so opening + Σ == closing.
		lines, err := tk.Store().DrillSplits(ctx, store.DrillFilter{
			Scope:     p.Scope,
			AccountID: p.Account,
			Currency:  ccy,
			From:      p.From,
			To:        p.To,
		})
		if err != nil {
			return Table{}, err
		}

		running := open
		for _, ln := range lines {
			running += ln.Amount
			t.Rows = append(t.Rows, Row{
				Cells: []Cell{
					DateCell(ln.Date).WithTxn(ln.TxnID),
					TextCell(lineDescription(ln, payees)),
					fundCell(ln.FundID, funds),
					MoneyCell(ln.Amount, ccy),
					MoneyCell(running, ccy),
				},
				Kind: RowData,
			})
		}

		// Closing row: the cumulative balance as of To. It EQUALS opening + Σlines
		// (running) by construction; it is drillable to its own as-of transactions.
		closeDrill := &Drill{
			Scope:      p.Scope,
			AccountIDs: []int64{p.Account},
			Currency:   ccy,
			Mode:       DrillAsOf,
			AsOf:       p.To,
		}
		t.Rows = append(t.Rows, Row{
			Cells: []Cell{
				DateCell(p.To),
				LabelCell("reports.account_ledger.closing"),
				BlankMoneyCell(),
				BlankMoneyCell(),
				MoneyCell(closeByCcy[ccy], ccy).WithDrill(closeDrill),
			},
			Kind: RowTotal,
		})
	}

	return t, nil
}

// lineDescription is a ledger line's Description cell text: the payee name when the
// split's transaction names one, else the split memo, else the transaction memo —
// mirroring the register's memo fallback (a payee-tagged line reads by its payee; an
// untagged line by its note).
func lineDescription(ln store.DrillRow, payees map[int64]string) string {
	if ln.PayeeID != nil {
		if name := payees[*ln.PayeeID]; name != "" {
			return name
		}
	}
	if ln.SplitMemo != "" {
		return ln.SplitMemo
	}
	return ln.TxnMemo
}

// fundCell builds the FUND column cell for a line: the fund's name (a stored proper
// noun, TEXT) for a restricted split, or the localized "Unrestricted" LABEL for a
// nil-fund split (the unrestricted group, D20 — a synthetic label, not a stored name,
// so it is a catalog key the renderer localizes).
func fundCell(fundID *int64, funds map[int64]string) Cell {
	if fundID == nil {
		return LabelCell("reports.account_ledger.unrestricted")
	}
	return TextCell(funds[*fundID])
}

// amtsByCurrency reduces a per-currency CurAmt slice (one account's balances) to a
// currency->minor map for direct lookup by section.
func amtsByCurrency(amts []CurAmt) map[string]int64 {
	out := make(map[string]int64, len(amts))
	for _, a := range amts {
		out[a.Currency] += a.Minor
	}
	return out
}

// unionCurrencies returns the sorted union of the currency keys of two maps (the
// account's opening and closing currencies), so a section is emitted for every
// currency the account holds at either end of the range.
func unionCurrencies(a, b map[string]int64) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for c := range a {
		seen[c] = true
	}
	for c := range b {
		seen[c] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// dayBefore returns the ISO date one calendar day before d (the opening-balance date =
// the day before the period's From, so the opening balance is cumulative strictly
// BEFORE the range). It follows compute.go's ISO-string date convention (time.Parse
// with the ISO layout); a malformed date passes through unchanged (the store's
// as-of query then treats it literally, degrading to an empty/whole result rather than
// erroring — the framework's forgiving-input posture).
func dayBefore(d string) string {
	tm, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return tm.AddDate(0, 0, -1).Format("2006-01-02")
}

// payeeNames returns id->name for every payee (bounded reference data, loaded once per
// report run) so a ledger line resolves its payee without a per-line join.
func payeeNames(ctx context.Context, st *store.Store) (map[int64]string, error) {
	ps, err := st.ListPayees(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(ps))
	for _, p := range ps {
		m[p.ID] = p.Name
	}
	return m, nil
}

// fundNames returns id->name for every fund (active AND closed — a historical line may
// reference a now-closed fund), loaded once per report run.
func fundNames(ctx context.Context, st *store.Store) (map[int64]string, error) {
	fs, err := st.ListFunds(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(fs))
	for _, f := range fs {
		m[f.ID] = f.Name
	}
	return m, nil
}
